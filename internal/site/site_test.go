// Package site checks the static homepage in site/ against its contract:
// no third-party origins, resolvable local references and accessible images.
package site

import (
	"bytes"
	"encoding/binary"
	"fmt"
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

var urlPattern = regexp.MustCompile(`(?i)(?:https?:)?//[^\s"'<>)\\]+`)

// allowedURL accepts GitHub links under this repository and the Pages site only.
func allowedURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	switch u.Host {
	case "github.com":
		return u.Path == "/Dunky13/agent-job-queue" || strings.HasPrefix(u.Path, "/Dunky13/agent-job-queue/")
	case "dunky13.github.io":
		return u.Path == "/agent-job-queue" || strings.HasPrefix(u.Path, "/agent-job-queue/")
	}
	return false
}

func TestAllowedURL(t *testing.T) {
	for _, ok := range []string{
		"https://github.com/Dunky13/agent-job-queue",
		"https://github.com/Dunky13/agent-job-queue/releases",
		"https://dunky13.github.io/agent-job-queue/",
	} {
		if !allowedURL(ok) {
			t.Errorf("%s should be allowed", ok)
		}
	}
	for _, bad := range []string{
		"https://github.co/Dunky13/agent-job-queue",
		"https://g",
		"https://github.com",
		"https://github.com/Other/repo",
		"https://github.com/Dunky13/agent-job-queue-evil",
		"https://github.com.evil.example/Dunky13/agent-job-queue",
		"http://github.com/Dunky13/agent-job-queue",
		"//github.com/Dunky13/agent-job-queue",
		"https://user@github.com/Dunky13/agent-job-queue",
		"https://dunky13.github.io/other",
		"https://evil.example/agent-job-queue/",
	} {
		if allowedURL(bad) {
			t.Errorf("%s should be rejected", bad)
		}
	}
}

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
		for _, m := range urlPattern.FindAllString(string(data), -1) {
			if !allowedURL(m) {
				t.Errorf("%s: disallowed URL %q", path, m)
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

// imageSize reads the pixel size of a PNG or WebP (lossy or extended) file.
func imageSize(path string) (int, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	switch filepath.Ext(path) {
	case ".png":
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		return cfg.Width, cfg.Height, err
	case ".webp":
		if len(data) < 30 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
			return 0, 0, fmt.Errorf("not a WebP file")
		}
		switch string(data[12:16]) {
		case "VP8 ":
			return int(binary.LittleEndian.Uint16(data[26:]) & 0x3fff), int(binary.LittleEndian.Uint16(data[28:]) & 0x3fff), nil
		case "VP8X":
			return int(data[24]) | int(data[25])<<8 | int(data[26])<<16 + 1, int(data[27]) | int(data[28])<<8 | int(data[29])<<16 + 1, nil
		}
		return 0, 0, fmt.Errorf("unsupported WebP chunk %q", data[12:16])
	}
	return 0, 0, fmt.Errorf("unsupported image type")
}

func TestImagesAreAccessibleAndSized(t *testing.T) {
	doc := parseHTML(t, "index.html")
	count := 0
	check := func(src string, wantW, wantH int) {
		w, h, err := imageSize(filepath.Join(siteDir, filepath.FromSlash(src)))
		if err != nil || w != wantW || h != wantH {
			t.Errorf("%s: declared %dx%d, file is %dx%d (%v)", src, wantW, wantH, w, h, err)
		}
	}
	og := map[string]string{}
	walk(doc, func(n *html.Node) {
		if n.Data == "meta" {
			if p, _ := attr(n, "property"); strings.HasPrefix(p, "og:image") {
				og[p] = attrOrEmpty(n, "content")
			}
		}
		if n.Data != "img" {
			return
		}
		count++
		src, _ := attr(n, "src")
		if _, ok := attr(n, "alt"); !ok {
			t.Errorf("%s: missing alt", src)
		}
		w, wErr := strconv.Atoi(attrOrEmpty(n, "width"))
		h, hErr := strconv.Atoi(attrOrEmpty(n, "height"))
		if wErr != nil || hErr != nil || w <= 0 || h <= 0 {
			t.Errorf("%s: needs positive integer width and height", src)
			return
		}
		check(src, w, h)
	})
	if count == 0 {
		t.Fatal("no images found")
	}
	w, wErr := strconv.Atoi(og["og:image:width"])
	h, hErr := strconv.Atoi(og["og:image:height"])
	if wErr != nil || hErr != nil || !strings.HasPrefix(og["og:image"], pagesURL) {
		t.Fatalf("og:image needs a Pages URL with integer width and height, got %v", og)
	}
	check(strings.TrimPrefix(og["og:image"], pagesURL), w, h)
}

// attrOrEmpty returns the attribute value, or "" when it is absent.
func attrOrEmpty(n *html.Node, key string) string {
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
		case "title", "main", "h1":
			seen[n.Data]++
		case "meta":
			if name, _ := attr(n, "name"); name == "description" && attrOrEmpty(n, "content") != "" {
				seen["description"]++
			}
			if p, _ := attr(n, "property"); strings.HasPrefix(p, "og:") {
				seen[p]++
			}
		case "a":
			if _, ok := attr(n, "data-download"); ok {
				if h := attrOrEmpty(n, "href"); h != "https://github.com/Dunky13/agent-job-queue/releases" {
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
}
