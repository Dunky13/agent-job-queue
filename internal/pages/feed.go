package pages

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"strconv"
	"strings"
)

const sparkleNamespace = "http://www.andymatuschak.org/xml-namespaces/sparkle"
const releasesURL = "https://github.com/" + repository + "/releases"

type enclosure struct {
	URL       string `xml:"url,attr"`
	Length    string `xml:"length,attr"`
	Type      string `xml:"type,attr"`
	Signature string `xml:"http://www.andymatuschak.org/xml-namespaces/sparkle edSignature,attr"`
}

type feedItem struct {
	Link      string    `xml:"link"`
	Version   string    `xml:"http://www.andymatuschak.org/xml-namespaces/sparkle version"`
	Short     string    `xml:"http://www.andymatuschak.org/xml-namespaces/sparkle shortVersionString"`
	MinSystem string    `xml:"http://www.andymatuschak.org/xml-namespaces/sparkle minimumSystemVersion"`
	Enclosure enclosure `xml:"enclosure"`
}

type feed struct {
	Items []feedItem `xml:"channel>item"`
}

// expectedItem is the one item a delivery of this build and ZIP must publish.
func expectedItem(data manifest, length int64, signature string) feedItem {
	return feedItem{
		Link:      releasesURL + "/tag/" + data.Tag,
		Version:   strconv.Itoa(data.Build),
		Short:     data.Version,
		MinSystem: "15.0",
		Enclosure: enclosure{URL: releasesURL + "/download/" + data.Tag + "/" + data.Asset, Length: strconv.FormatInt(length, 10), Type: "application/octet-stream", Signature: signature},
	}
}

// renderAppcast renders the unsigned single-item feed for the published ZIP.
func renderAppcast(data manifest, length int64, signature string) string {
	item := expectedItem(data, length, signature)
	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<rss version="2.0" xmlns:sparkle="`+sparkleNamespace+`">
  <channel>
    <title>Agent Queue</title>
    <link>%[1]s</link>
    <description>Agent Queue updates</description>
    <language>en</language>
    <item>
      <title>Version %[2]s (%[3]s)</title>
      <link>%[4]s</link>
      <sparkle:version>%[3]s</sparkle:version>
      <sparkle:shortVersionString>%[2]s</sparkle:shortVersionString>
      <sparkle:minimumSystemVersion>%[5]s</sparkle:minimumSystemVersion>
      <enclosure url="%[6]s" length="%[7]s" type="%[8]s" sparkle:edSignature="%[9]s"/>
    </item>
  </channel>
</rss>
`, releasesURL, html.EscapeString(item.Short), item.Version, html.EscapeString(item.Link), item.MinSystem, html.EscapeString(item.Enclosure.URL), item.Enclosure.Length, item.Enclosure.Type, item.Enclosure.Signature)
}

// parseFeed reads a feed after Sparkle re-serializes it. It returns its only
// item and the feed signature from the trailing sparkle-signatures comment.
func parseFeed(body []byte) (feedItem, string, error) {
	d := xml.NewDecoder(bytes.NewReader(body))
	var parsed feed
	var comment string
	roots := 0
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return feedItem{}, "", errors.New("The feed is not well-formed XML")
		}
		switch value := token.(type) {
		case xml.StartElement:
			if value.Name.Local != "rss" {
				return feedItem{}, "", errors.New("The feed root is not rss")
			}
			roots++
			if err := d.DecodeElement(&parsed, &value); err != nil {
				return feedItem{}, "", errors.New("The feed is not well-formed XML")
			}
		case xml.Comment:
			if strings.Contains(string(value), "sparkle-signatures:") {
				comment = string(value)
			}
		}
	}
	if roots != 1 || len(parsed.Items) != 1 {
		return feedItem{}, "", errors.New("The feed must contain exactly one item")
	}
	signature := ""
	for _, line := range strings.Split(comment, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "edSignature: "); ok {
			signature = value
		}
	}
	return parsed.Items[0], signature, nil
}

// signedFeedMatches requires the feed signature and exactly the produced item.
func signedFeedMatches(signed []byte, want feedItem) error {
	item, signature, err := parseFeed(signed)
	if err != nil {
		return err
	}
	if !signaturePattern.MatchString(signature) {
		return errors.New("The feed signature is missing")
	}
	if item != want || item.Version == "" || item.Short == "" || item.MinSystem == "" {
		return errors.New("The signed feed does not describe the produced release")
	}
	return nil
}

// sameFeed compares two feeds semantically, ignoring every signature, so the
// no-recommit decision never depends on how bytes were signed.
func sameFeed(a, b []byte) bool {
	left, _, err := parseFeed(a)
	if err != nil {
		return false
	}
	right, _, err := parseFeed(b)
	if err != nil {
		return false
	}
	left.Enclosure.Signature, right.Enclosure.Signature = "", ""
	return left == right
}
