// Package site checks the static homepage in site/ against its contract:
// no third-party origins, resolvable local references and accessible images.
package site

import (
	"image"
	_ "image/png"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

const (
	siteDir  = "../../site"
	pagesURL = "https://dunky13.github.io/agent-job-queue/"
)

var allowedOrigins = []string{
	"https://github.com/Dunky13/agent-job-queue",
	pagesURL,
}

var originPattern = regexp.MustCompile(`(?i)(?:https?:)?//[a-z0-9.-]+`)

func parseHTML(t *testing.T, name string) *html.Node {
	t.Helper()
	f, err := os.Open(filepath.Join(siteDir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	doc, err := html.Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func walk(n *html.Node, visit func(*html.Node)) {
	if n.Type == html.ElementNode {
		visit(n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, visit)
	}
}

func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func TestNoThirdPartyOrigins(t *testing.T) {
	err := filepath.WalkDir(siteDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		switch filepath.Ext(path) {
		case ".html", ".css", ".js":
		default:
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range originPattern.FindAllString(string(data), -1) {
			ok := false
			for _, prefix := range allowedOrigins {
				if strings.HasPrefix(strings.ToLower(m), strings.ToLower(prefix)) || strings.HasPrefix(strings.ToLower(prefix), strings.ToLower(m)) {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s: disallowed origin %q", path, m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLocalReferencesResolve(t *testing.T) {
	for _, page := range []string{"index.html", "404.html"} {
		doc := parseHTML(t, page)
		ids := map[string]bool{}
		var refs []string
		walk(doc, func(n *html.Node) {
			if id, ok := attr(n, "id"); ok {
				ids[id] = true
			}
			for _, key := range []string{"href", "src"} {
				if v, ok := attr(n, key); ok {
					refs = append(refs, v)
				}
			}
			if n.Data == "meta" {
				if p, _ := attr(n, "property"); p == "og:image" {
					v, _ := attr(n, "content")
					refs = append(refs, v)
				}
			}
		})
		if len(refs) == 0 {
			t.Fatalf("%s: no references found", page)
		}
		for _, ref := range refs {
			u, err := url.Parse(ref)
			if err != nil {
				t.Errorf("%s: bad reference %q", page, ref)
				continue
			}
			if u.Fragment != "" && u.Path == "" && u.Host == "" {
				if !ids[u.Fragment] {
					t.Errorf("%s: fragment #%s has no target", page, u.Fragment)
				}
				continue
			}
			local := ref
			switch {
			case strings.HasPrefix(ref, pagesURL):
				local = strings.TrimPrefix(ref, pagesURL)
			case u.Scheme != "" || u.Host != "":
				continue // external, covered by TestNoThirdPartyOrigins
			}
			local = strings.SplitN(local, "#", 2)[0]
			if local == "" || local == "./" {
				local = "index.html"
			}
			if _, err := os.Stat(filepath.Join(siteDir, filepath.FromSlash(local))); err != nil {
				t.Errorf("%s: %q does not resolve: %v", page, ref, err)
			}
		}
	}
}

func TestImagesAreAccessibleAndSized(t *testing.T) {
	doc := parseHTML(t, "index.html")
	count := 0
	walk(doc, func(n *html.Node) {
		if n.Data != "img" {
			return
		}
		count++
		src, _ := attr(n, "src")
		if _, ok := attr(n, "alt"); !ok {
			t.Errorf("%s: missing alt", src)
		}
		w, wErr := strconv.Atoi(mustAttr(n, "width"))
		h, hErr := strconv.Atoi(mustAttr(n, "height"))
		if wErr != nil || hErr != nil || w <= 0 || h <= 0 {
			t.Errorf("%s: needs positive integer width and height", src)
			return
		}
		if filepath.Ext(src) != ".png" {
			return
		}
		f, err := os.Open(filepath.Join(siteDir, filepath.FromSlash(src)))
		if err != nil {
			t.Errorf("%s: %v", src, err)
			return
		}
		defer f.Close()
		cfg, _, err := image.DecodeConfig(f)
		if err != nil || cfg.Width != w || cfg.Height != h {
			t.Errorf("%s: declared %dx%d, file is %dx%d (%v)", src, w, h, cfg.Width, cfg.Height, err)
		}
	})
	if count == 0 {
		t.Fatal("no images found")
	}
}

func mustAttr(n *html.Node, key string) string {
	v, _ := attr(n, key)
	return v
}

func TestPageMetadataAndLandmarks(t *testing.T) {
	doc := parseHTML(t, "index.html")
	seen := map[string]int{}
	walk(doc, func(n *html.Node) {
		switch n.Data {
		case "html":
			if l, _ := attr(n, "lang"); l == "" {
				t.Error("html needs lang")
			}
		case "title", "main", "h1", "header", "footer":
			seen[n.Data]++
		case "meta":
			if name, _ := attr(n, "name"); name == "description" && mustAttr(n, "content") != "" {
				seen["description"]++
			}
			if p, _ := attr(n, "property"); strings.HasPrefix(p, "og:") {
				seen[p]++
			}
		case "a":
			if _, ok := attr(n, "data-download"); ok {
				if h := mustAttr(n, "href"); h != "https://github.com/Dunky13/agent-job-queue/releases" {
					t.Errorf("download default href is %q", h)
				}
				seen["download"]++
			}
		}
	})
	for _, k := range []string{"title", "main", "h1", "description", "og:title", "og:description", "og:image", "download"} {
		if seen[k] != 1 {
			t.Errorf("%s: want exactly 1, got %d", k, seen[k])
		}
	}
	if _, err := os.Stat(filepath.Join(siteDir, "404.html")); err != nil {
		t.Error(err)
	}
}
