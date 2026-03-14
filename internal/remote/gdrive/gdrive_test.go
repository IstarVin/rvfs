package gdrive

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestIsRetryableStatus(t *testing.T) {
	t.Parallel()

	rateLimitedBody := []byte(`{"error":{"errors":[{"reason":"userRateLimitExceeded"}]}}`)
	notRateLimitedBody := []byte(`{"error":{"errors":[{"reason":"insufficientFilePermissions"}]}}`)

	assert.True(t, isRetryableStatus(http.StatusTooManyRequests, nil))
	assert.True(t, isRetryableStatus(http.StatusForbidden, rateLimitedBody))
	assert.False(t, isRetryableStatus(http.StatusForbidden, notRateLimitedBody))
	assert.True(t, isRetryableStatus(http.StatusServiceUnavailable, nil))
	assert.False(t, isRetryableStatus(http.StatusNotFound, nil))
}

func TestParseGoogleErrorReason(t *testing.T) {
	t.Parallel()

	body := []byte(`{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`)
	assert.Equal(t, "rateLimitExceeded", parseGoogleErrorReason(body))
	assert.Equal(t, "", parseGoogleErrorReason([]byte(`{"error":{}}`)))
	assert.Equal(t, "", parseGoogleErrorReason([]byte(`not-json`)))
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 3*time.Second, parseRetryAfter("3"))
	assert.Equal(t, time.Duration(0), parseRetryAfter(""))
	assert.Equal(t, time.Duration(0), parseRetryAfter("invalid"))

	future := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	d := parseRetryAfter(future)
	assert.Greater(t, d, time.Duration(0))
	assert.LessOrEqual(t, d, 3*time.Second)
}

func TestRetryDelay(t *testing.T) {
	t.Parallel()

	assert.Equal(t, initialRetryBackoff, retryDelay(0, 0))
	assert.Equal(t, maxRetryBackoff, retryDelay(10, 0))
	assert.Equal(t, 5*time.Second, retryDelay(0, 5*time.Second))
}
