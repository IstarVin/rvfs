package gdrive

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/remote"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	drive "google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

var driveScopes = []string{drive.DriveScope}

const PORT = 8089

// GDriveAdapter implements remote.RemoteAdapter for Google Drive.
type GDriveAdapter struct {
	srv       *drive.Service
	db        *cache.MetadataDB
	rootID    string // Drive ID of the configured root folder
	rootPath  string // logical root path (e.g. "Documents")
	tokenPath string
	oauthCfg  *oauth2.Config
}

// ensure interface compliance at compile time.
var _ remote.RemoteAdapter = (*GDriveAdapter)(nil)

// New creates a GDriveAdapter. It loads the saved token from tokenPath and
// initialises the Drive API client. Use Authenticate first if no token exists.
func New(clientID, clientSecret, tokenPath, rootPath string, db *cache.MetadataDB) (*GDriveAdapter, error) {
	cfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       driveScopes,
		Endpoint:     google.Endpoint,
		RedirectURL:  fmt.Sprintf("http://localhost:%d", PORT),
	}

	tok, err := loadToken(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("load token (run 'rvfs remote add' first): %w", err)
	}

	ctx := context.Background()
	client := cfg.Client(ctx, tok)

	srv, err := drive.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return nil, fmt.Errorf("create drive service: %w", err)
	}

	adapter := &GDriveAdapter{
		srv:       srv,
		db:        db,
		rootPath:  rootPath,
		tokenPath: tokenPath,
		oauthCfg:  cfg,
	}

	// Resolve the root folder ID.
	rootID, err := adapter.resolveRootID(context.Background())
	if err != nil {
		return nil, fmt.Errorf("resolve root %q: %w", rootPath, err)
	}
	adapter.rootID = rootID

	return adapter, nil
}

// Authenticate runs the interactive OAuth2 flow: starts a local server,
// opens the authorization URL, waits for the redirect callback, exchanges
// the authorization code for a token, and saves it.
func Authenticate(clientID, clientSecret, tokenPath string) error {
	cfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       driveScopes,
		Endpoint:     google.Endpoint,
		RedirectURL:  fmt.Sprintf("http://localhost:%d", PORT),
	}

	// Channel to receive the authorization code from the callback handler
	codeChan := make(chan string, 1)
	errChan := make(chan error, 1)

	// Start local HTTP server to handle the OAuth callback
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			errMsg := r.URL.Query().Get("error")
			if errMsg != "" {
				errChan <- fmt.Errorf("authorization denied: %s", errMsg)
			} else {
				errChan <- fmt.Errorf("no authorization code in callback")
			}
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, "Authorization failed: %s\n", r.URL.Query().Get("error"))
			return
		}
		codeChan <- code
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "Authorization successful! You can close this window.")
	})

	server := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", PORT),
		Handler: mux,
	}

	// Start server in a goroutine
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errChan <- fmt.Errorf("server error: %w", err)
		}
	}()

	// Print authorization URL
	url := cfg.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("Opening browser for authorization...\n\nIf browser doesn't open, visit:\n%s\n\n", url)

	// Wait for authorization code or error
	var code string
	select {
	case code = <-codeChan:
		// Code received successfully
	case err := <-errChan:
		server.Close()
		return err
	}

	// Shutdown the server
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(ctx)

	// Exchange code for token
	tok, err := cfg.Exchange(context.Background(), code)
	if err != nil {
		return fmt.Errorf("exchange token: %w", err)
	}

	return saveToken(tokenPath, tok)
}

// ---------- RemoteAdapter implementation ----------

func (g *GDriveAdapter) List(ctx context.Context, dirPath string) ([]remote.FileInfo, error) {
	parentID, err := g.resolveID(ctx, dirPath)
	if err != nil {
		return nil, err
	}

	var result []remote.FileInfo
	query := fmt.Sprintf("'%s' in parents and trashed = false", escapeQuery(parentID))
	fields := "nextPageToken, files(id, name, size, mimeType, modifiedTime, md5Checksum)"

	pageToken := ""
	for {
		call := g.srv.Files.List().
			Q(query).
			Fields(googleapi.Field(fields)).
			PageSize(1000)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}

		fl, err := call.Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("list %q: %w", dirPath, err)
		}

		for _, f := range fl.Files {
			// Skip Google Docs native formats (no downloadable content).
			if isGoogleDoc(f.MimeType) {
				continue
			}

			childPath := joinPath(dirPath, f.Name)
			isDir := f.MimeType == "application/vnd.google-apps.folder"

			var mtime time.Time
			if f.ModifiedTime != "" {
				mtime, _ = time.Parse(time.RFC3339, f.ModifiedTime)
			}

			result = append(result, remote.FileInfo{
				Path:     childPath,
				Name:     f.Name,
				Size:     f.Size,
				IsDir:    isDir,
				Mtime:    mtime,
				Checksum: f.Md5Checksum,
			})

			// Cache the path→ID mapping.
			_ = g.db.PutDriveID(&cache.DrivePathEntry{
				Path:     childPath,
				DriveID:  f.Id,
				ETag:     "",
				LastSeen: time.Now().Unix(),
			})
		}

		pageToken = fl.NextPageToken
		if pageToken == "" {
			break
		}
	}

	return result, nil
}

func (g *GDriveAdapter) Stat(ctx context.Context, filePath string) (remote.FileInfo, error) {
	id, err := g.resolveID(ctx, filePath)
	if err != nil {
		return remote.FileInfo{}, err
	}

	f, err := g.srv.Files.Get(id).
		Fields("id, name, size, mimeType, modifiedTime, md5Checksum").
		Context(ctx).
		Do()
	if err != nil {
		return remote.FileInfo{}, fmt.Errorf("stat %q: %w", filePath, err)
	}

	var mtime time.Time
	if f.ModifiedTime != "" {
		mtime, _ = time.Parse(time.RFC3339, f.ModifiedTime)
	}

	return remote.FileInfo{
		Path:     filePath,
		Name:     f.Name,
		Size:     f.Size,
		IsDir:    f.MimeType == "application/vnd.google-apps.folder",
		Mtime:    mtime,
		Checksum: f.Md5Checksum,
	}, nil
}

func (g *GDriveAdapter) Get(ctx context.Context, filePath string, dest io.Writer) error {
	id, err := g.resolveID(ctx, filePath)
	if err != nil {
		return err
	}

	resp, err := g.srv.Files.Get(id).Context(ctx).Download()
	if err != nil {
		return fmt.Errorf("download %q: %w", filePath, err)
	}
	defer resp.Body.Close()

	_, err = io.Copy(dest, resp.Body)
	return err
}

func (g *GDriveAdapter) GetRange(ctx context.Context, filePath string, offset, length int64, dest io.Writer) error {
	id, err := g.resolveID(ctx, filePath)
	if err != nil {
		return err
	}

	// Build a range request manually via the HTTP client.
	url := fmt.Sprintf("https://www.googleapis.com/drive/v3/files/%s?alt=media", id)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	endByte := offset + length - 1
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, endByte))

	// Use the oauth2 http client embedded in the service.
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("get range %q: %w", filePath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get range %q: HTTP %d", filePath, resp.StatusCode)
	}

	_, err = io.Copy(dest, resp.Body)
	return err
}

func (g *GDriveAdapter) Put(ctx context.Context, filePath string, src io.Reader, size int64, mtime time.Time) error {
	dirPath, name := path.Split(filePath)
	dirPath = strings.TrimSuffix(dirPath, "/")

	parentID, err := g.resolveID(ctx, dirPath)
	if err != nil {
		return err
	}

	// Check if file already exists (update vs create).
	existingID, _ := g.resolveID(ctx, filePath)

	meta := &drive.File{
		Name:         name,
		ModifiedTime: mtime.UTC().Format(time.RFC3339),
	}

	if existingID != "" {
		// Update existing file.
		_, err = g.srv.Files.Update(existingID, meta).
			Context(ctx).
			Media(src).
			Do()
	} else {
		// Create new file.
		meta.Parents = []string{parentID}
		_, err = g.srv.Files.Create(meta).
			Context(ctx).
			Media(src).
			Do()
	}
	if err != nil {
		return fmt.Errorf("put %q: %w", filePath, err)
	}
	return nil
}

func (g *GDriveAdapter) Delete(ctx context.Context, filePath string) error {
	id, err := g.resolveID(ctx, filePath)
	if err != nil {
		return err
	}

	if err := g.srv.Files.Delete(id).Context(ctx).Do(); err != nil {
		return fmt.Errorf("delete %q: %w", filePath, err)
	}

	_ = g.db.DeleteDriveID(filePath)
	return nil
}

func (g *GDriveAdapter) Mkdir(ctx context.Context, dirPath string) error {
	parentPath, name := path.Split(dirPath)
	parentPath = strings.TrimSuffix(parentPath, "/")

	parentID, err := g.resolveID(ctx, parentPath)
	if err != nil {
		return err
	}

	meta := &drive.File{
		Name:     name,
		MimeType: "application/vnd.google-apps.folder",
		Parents:  []string{parentID},
	}

	f, err := g.srv.Files.Create(meta).Fields("id").Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("mkdir %q: %w", dirPath, err)
	}

	_ = g.db.PutDriveID(&cache.DrivePathEntry{
		Path:     dirPath,
		DriveID:  f.Id,
		LastSeen: time.Now().Unix(),
	})
	return nil
}

func (g *GDriveAdapter) Rename(ctx context.Context, src, dst string) error {
	id, err := g.resolveID(ctx, src)
	if err != nil {
		return err
	}

	// Get current parents.
	f, err := g.srv.Files.Get(id).Fields("parents").Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("rename get parents %q: %w", src, err)
	}

	newParentPath, newName := path.Split(dst)
	newParentPath = strings.TrimSuffix(newParentPath, "/")
	newParentID, err := g.resolveID(ctx, newParentPath)
	if err != nil {
		return err
	}

	oldParents := strings.Join(f.Parents, ",")
	_, err = g.srv.Files.Update(id, &drive.File{Name: newName}).
		AddParents(newParentID).
		RemoveParents(oldParents).
		Context(ctx).
		Do()
	if err != nil {
		return fmt.Errorf("rename %q→%q: %w", src, dst, err)
	}

	_ = g.db.DeleteDriveID(src)
	_ = g.db.PutDriveID(&cache.DrivePathEntry{
		Path:     dst,
		DriveID:  id,
		LastSeen: time.Now().Unix(),
	})
	return nil
}

func (g *GDriveAdapter) Probe(ctx context.Context) error {
	_, err := g.srv.About.Get().Fields("user").Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	return nil
}

func (g *GDriveAdapter) Quota(ctx context.Context) (remote.QuotaInfo, error) {
	about, err := g.srv.About.Get().Fields("storageQuota(limit,usage)").Context(ctx).Do()
	if err != nil {
		return remote.QuotaInfo{}, fmt.Errorf("quota: %w", err)
	}
	if about.StorageQuota == nil || about.StorageQuota.Limit <= 0 {
		return remote.QuotaInfo{}, nil
	}

	total := about.StorageQuota.Limit
	used := about.StorageQuota.Usage

	return remote.QuotaInfo{
		TotalBytes:     total,
		UsedBytes:      used,
		AvailableBytes: total - used,
	}.Normalized(), nil
}

func (g *GDriveAdapter) SupportsRange() bool {
	return true
}

// ---------- internal helpers ----------

// resolveRootID resolves the configured rootPath to a Drive folder ID.
// An empty rootPath maps to "root" (the user's My Drive).
func (g *GDriveAdapter) resolveRootID(ctx context.Context) (string, error) {
	if g.rootPath == "" {
		return "root", nil
	}

	parts := strings.Split(g.rootPath, "/")
	parentID := "root"
	accumulated := ""

	for _, part := range parts {
		if part == "" {
			continue
		}
		if accumulated == "" {
			accumulated = part
		} else {
			accumulated = accumulated + "/" + part
		}

		// Check DB cache first.
		cached, err := g.db.GetDriveID(accumulated)
		if err == nil && cached != "" {
			parentID = cached
			continue
		}

		// Walk via API.
		query := fmt.Sprintf("'%s' in parents and name = '%s' and mimeType = 'application/vnd.google-apps.folder' and trashed = false",
			escapeQuery(parentID), escapeQuery(part))
		fl, err := g.srv.Files.List().Q(query).Fields("files(id)").PageSize(1).Context(ctx).Do()
		if err != nil {
			return "", fmt.Errorf("resolve root segment %q: %w", part, err)
		}
		if len(fl.Files) == 0 {
			return "", fmt.Errorf("root folder %q not found (segment %q)", g.rootPath, part)
		}
		parentID = fl.Files[0].Id

		_ = g.db.PutDriveID(&cache.DrivePathEntry{
			Path:     accumulated,
			DriveID:  parentID,
			LastSeen: time.Now().Unix(),
		})
	}

	return parentID, nil
}

// resolveID resolves a relative path (relative to rootPath) to a Drive file ID.
// Empty path resolves to rootID.
func (g *GDriveAdapter) resolveID(ctx context.Context, relPath string) (string, error) {
	if relPath == "" {
		return g.rootID, nil
	}

	// Check DB cache.
	fullPath := relPath
	cached, err := g.db.GetDriveID(fullPath)
	if err == nil && cached != "" {
		return cached, nil
	}

	// Walk from root.
	parts := strings.Split(relPath, "/")
	parentID := g.rootID
	accumulated := ""

	for i, part := range parts {
		if part == "" {
			continue
		}
		if accumulated == "" {
			accumulated = part
		} else {
			accumulated = accumulated + "/" + part
		}

		// Check DB cache for intermediate segments.
		cached, err := g.db.GetDriveID(accumulated)
		if err == nil && cached != "" {
			parentID = cached
			continue
		}

		// Query API.
		query := fmt.Sprintf("'%s' in parents and name = '%s' and trashed = false",
			escapeQuery(parentID), escapeQuery(part))

		// For intermediate segments, restrict to folders.
		if i < len(parts)-1 {
			query += " and mimeType = 'application/vnd.google-apps.folder'"
		}

		fl, err := g.srv.Files.List().Q(query).Fields("files(id)").PageSize(1).Context(ctx).Do()
		if err != nil {
			return "", fmt.Errorf("resolve %q segment %q: %w", relPath, part, err)
		}
		if len(fl.Files) == 0 {
			return "", fmt.Errorf("path %q not found (segment %q)", relPath, part)
		}
		parentID = fl.Files[0].Id

		_ = g.db.PutDriveID(&cache.DrivePathEntry{
			Path:     accumulated,
			DriveID:  parentID,
			LastSeen: time.Now().Unix(),
		})
	}

	return parentID, nil
}

// httpClient returns the OAuth2-authenticated HTTP client from the Drive service.
func (g *GDriveAdapter) httpClient() *http.Client {
	tok, err := loadToken(g.tokenPath)
	if err != nil {
		slog.Warn("gdrive: failed to reload token for range request", "err", err)
	}
	return g.oauthCfg.Client(context.Background(), tok)
}

// escapeQuery escapes a string for use in Drive API query parameters.
func escapeQuery(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return s
}

// isGoogleDoc returns true for native Google Docs/Sheets/Slides/etc MIME types
// that cannot be downloaded as-is.
func isGoogleDoc(mimeType string) bool {
	return strings.HasPrefix(mimeType, "application/vnd.google-apps.") &&
		mimeType != "application/vnd.google-apps.folder"
}

// joinPath combines a directory path and filename.
func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}
