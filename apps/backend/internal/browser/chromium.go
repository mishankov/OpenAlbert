package browser

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"strings"
	"sync"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/integrations"
)

type Config struct {
	validate   func(context.Context, string) error
	Executable string
	NoSandbox  bool
}

func New(ctx context.Context, c Config) (*Manager, error) {
	if c.Executable == "" {
		for _, name := range []string{"chromium", "chromium-browser", "google-chrome"} {
			if file, err := exec.LookPath(name); err == nil {
				c.Executable = file
				break
			}
		}
	}
	if c.Executable == "" {
		return nil, errors.New("Chromium is missing; set BROWSER_EXECUTABLE or disable BROWSER_ENABLED")
	}
	if _, err := exec.LookPath(c.Executable); err != nil {
		return nil, errors.New("BROWSER_EXECUTABLE is not executable")
	}
	proxy, err := newPublicProxy()
	if err != nil {
		return nil, err
	}
	m, err := newManager(ctx, 5, 1, func(root context.Context) (driver, error) { return launch(root, c, proxy.URL()) })
	if err != nil {
		proxy.Close()
		return nil, err
	}
	m.validate = integrations.ValidatePublicURL
	m.proxy = proxy
	m.logger = slog.Default()
	return m, nil
}
func token() string { var b [12]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }

type tab struct {
	ctx    context.Context
	cancel context.CancelFunc
	refs   map[string]bool
}
type chromium struct {
	validate                func(context.Context, string) error
	ctx                     context.Context
	cancel, allocatorCancel context.CancelFunc
	id                      string
	tabs                    map[string]*tab
	once                    sync.Once
}

func launch(ctx context.Context, c Config, proxy string) (driver, error) {
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts, chromedp.NoInheritEnv, chromedp.Env("PATH=/usr/bin:/bin", "HOME=/tmp", "LANG=C.UTF-8"), chromedp.ExecPath(c.Executable), chromedp.ProxyServer(proxy), chromedp.Flag("proxy-bypass-list", "<-loopback>"), chromedp.Flag("host-resolver-rules", "MAP * ~NOTFOUND, EXCLUDE 127.0.0.1"), chromedp.Flag("disable-quic", true), chromedp.Flag("force-webrtc-ip-handling-policy", "disable_non_proxied_udp"), chromedp.Flag("disable-popup-blocking", false), chromedp.WindowSize(1280, 900))
	if c.NoSandbox {
		opts = append(opts, chromedp.NoSandbox)
	}
	alloc, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	root, cancel := chromedp.NewContext(alloc, chromedp.WithLogf(func(string, ...any) {}), chromedp.WithErrorf(func(string, ...any) {}))
	// The initial Run owns the browser lifetime: never give that Run an Activity deadline.
	timer := time.AfterFunc(25*time.Second, cancel)
	err := chromedp.Do(root, chromedp.EmulateViewport(1280, 900))
	timer.Stop()
	if err != nil {
		cancel()
		allocCancel()
		return nil, errors.New("Chromium startup failed")
	}
	_, err = chromedp.CallBrowser(root, cdpbrowser.SetDownloadBehavior, cdpbrowser.SetDownloadBehaviorParams{Behavior: cdpbrowser.SetDownloadBehaviorBehaviorDeny})
	if err != nil {
		cancel()
		allocCancel()
		return nil, errors.New("Chromium download policy failed")
	}
	if c.validate == nil {
		c.validate = integrations.ValidatePublicURL
	}
	return &chromium{validate: c.validate, ctx: root, cancel: cancel, allocatorCancel: allocCancel, id: token(), tabs: map[string]*tab{}}, nil
}
func (c *chromium) Close() { c.once.Do(func() { c.cancel(); c.allocatorCancel() }) }
func (t *tab) runContext(ctx context.Context) (context.Context, func()) {
	operation, cancel := context.WithCancel(t.ctx)
	stop := context.AfterFunc(ctx, cancel)
	return operation, func() { stop(); cancel() }
}
func (c *chromium) Execute(ctx context.Context, req domain.BrowserRequest) (domain.BrowserResult, error) {
	out := domain.BrowserResult{InstanceID: c.id}
	if req.Action == "tabs" {
		for id, t := range c.tabs {
			op, cancel := t.runContext(ctx)
			title, err := chromedp.Run(op, chromedp.Title())
			location, _ := chromedp.Run(op, chromedp.Location())
			cancel()
			if err == nil {
				out.Pages = append(out.Pages, domain.BrowserPage{ID: id, URL: location, Title: title})
			}
		}
		return out, nil
	}
	if req.Action == "open" {
		if err := c.validate(ctx, req.URL); err != nil {
			return out, err
		}
		if len(c.tabs) >= 8 {
			return out, errors.New("maximum eight pages per browser; close an unused page")
		}
		page, cancel := chromedp.NewContext(c.ctx)
		id := token()
		t := &tab{ctx: page, cancel: cancel}
		c.tabs[id] = t
		// Establish each target before using a request-scoped cancellation context.
		stopCreation := context.AfterFunc(ctx, cancel)
		err := chromedp.Do(page, chromedp.EmulateViewport(1280, 900))
		stopCreation()
		if err != nil {
			delete(c.tabs, id)
			cancel()
			return out, errors.New("browser page creation failed")
		}
		req.PageID = id
		req.Action = "navigate"
	}
	t := c.tabs[req.PageID]
	if t == nil {
		return out, errors.New("page not found in this browser; use browser_open (page IDs expire after browser restart)")
	}
	op, cancel := t.runContext(ctx)
	defer cancel()
	var err error
	switch req.Action {
	case "navigate":
		if err = c.validate(ctx, req.URL); err == nil {
			err = chromedp.Do(op, chromedp.Navigate(req.URL))
		}
	case "snapshot":
	case "click", "fill":
		if !t.refs[req.Ref] {
			return out, errors.New("unknown or stale element reference; take a fresh browser_snapshot")
		}
		selector := chromedp.CSS(`[data-openalbert-ref="` + req.Ref + `"]`)
		if req.Action == "click" {
			err = chromedp.Do(op, chromedp.Click(selector))
		} else {
			typ, e := chromedp.Run(op, chromedp.AttributeValue(selector, "type"))
			if e != nil {
				return out, errors.New("element no longer exists; take a fresh snapshot")
			}
			if typ.Value == "password" {
				return out, errors.New("authenticated services are not supported")
			}
			err = chromedp.Do(op, chromedp.SetValue(selector, req.Text))
		}
	case "wait":
		if req.Milliseconds < 0 || req.Milliseconds > 5000 {
			return out, errors.New("wait must be between 0 and 5000 milliseconds")
		}
		timer := time.NewTimer(time.Duration(req.Milliseconds) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-timer.C:
		}
	case "screenshot":
		out.Data, err = chromedp.Run(op, chromedp.CaptureScreenshot())
		out.MIME = "image/png"
		out.Filename = "browser.png"
	case "close_page":
		t.cancel()
		delete(c.tabs, req.PageID)
		out.Text = "Page closed."
		return out, nil
	default:
		return out, errors.New("unknown browser action")
	}
	if err != nil {
		return out, errors.New("browser operation failed; for clicks the action may already have occurred, inspect a fresh snapshot before retrying")
	}
	snapshot, err := c.snapshot(op, t, req.PageID)
	if err != nil {
		return out, err
	}
	snapshot.InstanceID = c.id
	snapshot.Data = out.Data
	snapshot.MIME = out.MIME
	snapshot.Filename = out.Filename
	return snapshot, nil
}
func (c *chromium) snapshot(ctx context.Context, t *tab, id string) (domain.BrowserResult, error) {
	prefix := token()
	// Return a bounded visible-text observation and visible interactive elements.
	script := `(()=>{const prefix=` + stringJSON(prefix) + `;const elements=[];for(const el of document.querySelectorAll('a[href],button,input:not([type=hidden]),textarea,select,[role=button],[role=link]')){if(elements.length>=80)break;const r=el.getBoundingClientRect();const s=getComputedStyle(el);if(!r.width||!r.height||s.visibility==='hidden'||s.display==='none')continue;const ref=prefix+'-'+elements.length;el.setAttribute('data-openalbert-ref',ref);elements.push({ref,role:el.getAttribute('role')||el.tagName.toLowerCase(),name:(el.getAttribute('aria-label')||el.innerText||el.getAttribute('placeholder')||el.getAttribute('name')||'').slice(0,180),url:el.href||''});}return {url:location.href,title:document.title,text:(document.body?.innerText||'').slice(0,12000),elements};})()`
	out, err := chromedp.Run(ctx, chromedp.Evaluate[domain.BrowserResult](script))
	if err != nil {
		return out, errors.New("cannot observe browser page; reopen it if Chromium restarted")
	}
	if err = c.validate(ctx, out.URL); err != nil {
		return domain.BrowserResult{}, errors.New("browser is not on a public HTTP(S) page")
	}
	t.refs = map[string]bool{}
	for _, el := range out.Elements {
		t.refs[el.Ref] = true
	}
	out.PageID = id
	return out, nil
}
func stringJSON(s string) string { b, _ := json.Marshal(s); return string(b) }
func downloadPublic(ctx context.Context, req domain.BrowserRequest) (domain.BrowserResult, error) {
	out := domain.BrowserResult{}
	if err := integrations.ValidatePublicURL(ctx, req.URL); err != nil {
		return out, err
	}
	transport := &http.Transport{DialContext: integrations.DialPublic}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return integrations.ValidatePublicURL(r.Context(), r.URL.String())
	}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	if err != nil {
		return out, err
	}
	response, err := client.Do(request)
	if err != nil {
		return out, errors.New("public file download failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return out, errors.New("public file download rejected")
	}
	out.Data, err = io.ReadAll(io.LimitReader(response.Body, (20<<20)+1))
	if err != nil || len(out.Data) > 20<<20 {
		return domain.BrowserResult{}, errors.New("download failed or exceeds 20 MiB")
	}
	u, _ := url.Parse(req.URL)
	out.Filename = path.Base(u.Path)
	if out.Filename == "." || out.Filename == "/" || out.Filename == "" {
		out.Filename = "download.bin"
	}
	out.MIME = strings.Split(response.Header.Get("Content-Type"), ";")[0]
	if out.MIME == "" {
		out.MIME = "application/octet-stream"
	}
	out.URL = response.Request.URL.String()
	return out, nil
}

func (c *chromium) Alive() bool {
	if c.ctx.Err() != nil {
		return false
	}
	select {
	case <-chromedp.FromContext(c.ctx).Browser.LostConnection:
		return false
	default:
		return true
	}
}
