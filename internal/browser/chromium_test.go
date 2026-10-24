package browser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"openalbert/internal/domain"
)

func TestProxyRejectsPrivateDestinationsAndConnectPorts(t *testing.T) {
	p, err := newPublicProxy()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	proxyURL, _ := url.Parse(p.URL())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: time.Second}
	for _, target := range []string{"http://127.0.0.1/", "http://169.254.169.254/latest/meta-data", "http://localhost/", "http://[::1]/", "http://192.168.1.1/", "http://example.com:5432/"} {
		response, err := client.Get(target)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 403 {
			t.Fatalf("private destination allowed: %s %d", target, response.StatusCode)
		}
	}
	conn, err := net.Dial("tcp", p.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "CONNECT 127.0.0.1:5432 HTTP/1.1\r\nHost: 127.0.0.1:5432\r\n\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 128)
	n, _ := conn.Read(b)
	if !bytes.Contains(b[:n], []byte("403")) {
		t.Fatal("private CONNECT allowed")
	}
}
func TestChromiumIntegration(t *testing.T) {
	binary := os.Getenv("OPENALBERT_TEST_CHROMIUM")
	if binary == "" {
		t.Skip("set OPENALBERT_TEST_CHROMIUM to run real browser tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	var privateHits atomic.Int32
	var privateURL atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/private" {
			privateHits.Add(1)
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!doctype html><img src="%s"><title>Browser test</title><h1>Dynamic page</h1><input aria-label="Name"><button onclick="document.querySelector('h1').innerText='Hello '+document.querySelector('input').value">Greet</button><a href="/second">Second</a><script>document.querySelector('h1').innerText='JavaScript ready';</script>`, privateURL.Load().(string))
	}))
	defer server.Close()
	privateURL.Store(server.URL + "/private")
	destination := strings.TrimPrefix(server.URL, "http://")
	proxy, err := proxyWithDial(func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "public.test:80" {
			return nil, errors.New("test destination blocked")
		}
		return (&net.Dialer{}).DialContext(ctx, network, destination)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	cfg := Config{Executable: binary, NoSandbox: os.Getenv("OPENALBERT_TEST_NO_SANDBOX") == "true", validate: func(ctx context.Context, raw string) error {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "http" || u.Host != "public.test" {
			return errors.New("not a public test URL")
		}
		return nil
	}}
	m := testManager(t, func(root context.Context) (driver, error) { return launch(root, cfg, proxy.URL()) })
	open := func(id string) domain.BrowserResult {
		t.Helper()
		r, err := m.Execute(ctx, owner(id, false), domain.BrowserRequest{Action: "open", URL: "http://public.test/"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Fresh || r.Title != "Browser test" || !strings.Contains(r.Text, "JavaScript ready") {
			t.Fatalf("dynamic page not rendered: %+v", r)
		}
		return r
	}
	first := open("one")
	second := open("two")
	if first.InstanceID == second.InstanceID || first.PageID == second.PageID {
		t.Fatal("sessions shared a browser")
	}
	var field, button string
	for _, el := range first.Elements {
		if el.Name == "Name" {
			field = el.Ref
		}
	}
	changed, err := m.Execute(ctx, owner("one", false), domain.BrowserRequest{Action: "fill", PageID: first.PageID, Ref: field, Text: "Denis"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, el := range changed.Elements {
		if el.Name == "Greet" {
			button = el.Ref
		}
	}
	changed, err = m.Execute(ctx, owner("one", false), domain.BrowserRequest{Action: "click", PageID: first.PageID, Ref: button}, nil)
	if err != nil || !strings.Contains(changed.Text, "Hello Denis") {
		t.Fatalf("click/fill failed: %+v %v", changed, err)
	}
	if _, err = m.Execute(ctx, owner("one", false), domain.BrowserRequest{Action: "click", PageID: first.PageID, Ref: button}, nil); err == nil {
		t.Fatal("stale reference reused")
	}
	if _, err = m.Execute(ctx, owner("two", false), domain.BrowserRequest{Action: "snapshot", PageID: first.PageID}, nil); err == nil {
		t.Fatal("page leaked between sessions")
	}
	shot, err := m.Execute(ctx, owner("one", false), domain.BrowserRequest{Action: "screenshot", PageID: first.PageID}, nil)
	if err != nil || !bytes.HasPrefix(shot.Data, []byte("\x89PNG")) {
		t.Fatalf("screenshot failed %v", err)
	}
	// A second managed tab shares only its owner's Chromium process.
	otherTab := openExisting(t, ctx, m, "one")
	if otherTab.InstanceID != first.InstanceID || otherTab.PageID == first.PageID {
		t.Fatal("tab was not created in existing instance")
	}
	m.mu.Lock()
	process := chromedp.FromContext(m.entries["one"].driver.(*chromium).ctx).Browser.Process()
	m.mu.Unlock()
	m.CloseSession("one")
	if _, err = os.Stat(fmt.Sprintf("/proc/%d", process.Pid)); runtime.GOOS == "linux" && err == nil {
		t.Fatal("closed Chromium process remains")
	}
	still, err := m.Execute(ctx, owner("two", false), domain.BrowserRequest{Action: "snapshot", PageID: second.PageID}, nil)
	if err != nil || !strings.Contains(still.Text, "JavaScript ready") {
		t.Fatal("closing one browser affected another", err)
	}
	if _, err = m.Execute(ctx, owner("two", false), domain.BrowserRequest{Action: "navigate", PageID: second.PageID, URL: "file:///etc/passwd"}, nil); err == nil {
		t.Fatal("non-public navigation allowed")
	}
	if privateHits.Load() != 0 {
		t.Fatal("Chromium bypassed proxy for a private subresource")
	}
	t.Log("Independent processes, JavaScript, fill, click, stale refs, tabs, screenshot, private subresource blocking and cleanup verified")
}
func openExisting(t *testing.T, ctx context.Context, m *Manager, id string) domain.BrowserResult {
	t.Helper()
	r, err := m.Execute(ctx, owner(id, false), domain.BrowserRequest{Action: "open", URL: "http://public.test/second"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
