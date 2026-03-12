package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadOnlyFromContext(t *testing.T) {
	tests := []struct {
		mode string
		want bool
	}{
		{ModeObserve, true},
		{ModeFull, false},
		{ModeWrite, false},
		{"", false},
	}

	for _, tc := range tests {
		ctx := context.WithValue(context.Background(), ContextMode, tc.mode)
		if got := ReadOnlyFromContext(ctx); got != tc.want {
			t.Fatalf("mode=%q: got %v want %v", tc.mode, got, tc.want)
		}
	}
}

func TestReadOnlyGuard_BlocksObserveMode(t *testing.T) {
	reached := false
	handler := ReadOnlyGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), ContextMode, ModeObserve))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	if reached {
		t.Fatal("inner handler should not be reached")
	}
}
