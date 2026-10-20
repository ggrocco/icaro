package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSocketHandlerPprofOnlyWhenEnabled(t *testing.T) {
	app := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	for _, tc := range []struct {
		on   bool
		path string
		want int
	}{
		{false, "/debug/pprof/", http.StatusTeapot},
		{true, "/debug/pprof/", http.StatusOK},
		{true, "/api/v1/runs", http.StatusTeapot},
	} {
		rec := httptest.NewRecorder()
		socketHandler(app, tc.on).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.want {
			t.Fatalf("pprof=%v %s = %d, want %d", tc.on, tc.path, rec.Code, tc.want)
		}
	}
}
