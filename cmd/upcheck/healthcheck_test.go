package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRunHealthcheck 은 200 이면 nil, 그 밖이면 에러인지 본다.
func TestRunHealthcheck(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"준비됨", http.StatusOK, false},
		{"준비 안 됨", http.StatusServiceUnavailable, true},
		{"서버 오류", http.StatusInternalServerError, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/readyz" {
					t.Errorf("경로 %q — /readyz 를 찔러야 한다", r.URL.Path)
				}
				w.WriteHeader(c.status)
			}))
			defer srv.Close()

			addr := strings.TrimPrefix(srv.URL, "http://")
			err := runHealthcheck(addr)
			if c.wantErr && err == nil {
				t.Error("에러를 기대했는데 성공했다")
			}
			if !c.wantErr && err != nil {
				t.Errorf("성공을 기대했는데 에러: %v", err)
			}
		})
	}
}

// TestRunHealthcheckResolvesBindAddress 는 ":8485" 나 "0.0.0.0:8485" 같은 bind 주소를 자기 자신으로 바꿔 부르는지 본다.
func TestRunHealthcheckResolvesBindAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	_, port, _ := net.SplitHostPort(ln.Addr().String())

	for _, addr := range []string{":" + port, "0.0.0.0:" + port, "127.0.0.1:" + port} {
		t.Run(addr, func(t *testing.T) {
			if err := runHealthcheck(addr); err != nil {
				t.Errorf("%s → %v", addr, err)
			}
		})
	}
}

func TestRunHealthcheckRejectsBadAddress(t *testing.T) {
	for _, addr := range []string{"", "포트없음"} {
		if err := runHealthcheck(addr); err == nil {
			t.Errorf("%q 는 거부해야 한다", addr)
		}
	}
}
