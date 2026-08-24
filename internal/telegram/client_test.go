package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client := NewClient("test-token", nil)
	client.baseURL = server.URL
	return client
}

func TestCallRetriesOn429AndHonorsRetryAfter(t *testing.T) {
	var calls int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 1","parameters":{"retry_after":1}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"username":"bot"}}`))
	})

	start := time.Now()
	if _, err := client.GetMe(context.Background()); err != nil {
		t.Fatalf("GetMe after 429 failed: %v", err)
	}

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected 2 attempts, got %d", got)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("expected to wait retry_after (>=1s), waited %s", elapsed)
	}
}

func TestCallDoesNotRetryClientError(t *testing.T) {
	var calls int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`))
	})

	err := client.SendMessage(context.Background(), 42, "text", nil)
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
	if IsRateLimit(err) {
		t.Fatalf("400 must not be reported as rate limit: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("400 must not be retried, got %d attempts", got)
	}
}

func TestCallRetriesOnServerError(t *testing.T) {
	var calls int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>bad gateway</html>"))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	})

	if err := client.ApproveChatJoinRequest(context.Background(), -100, 7); err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
}

func TestAuthErrorIsDetected(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
	})

	_, err := client.GetMe(context.Background())
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if !IsAuthError(err) {
		t.Fatalf("401 must be reported as auth error: %v", err)
	}
}
