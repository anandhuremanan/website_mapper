// Package classify decides what kind of resource a discovered URL is.
//
// Classification is evidence-based and always returns the reason for its
// decision. A path that merely looks like an API (e.g. contains /api/) is
// reported as API-like with evidence saying it was not verified.
package classify

import (
	"path"
	"regexp"
	"strings"

	"websitemapper/internal/discovery"
)

// Type is the broad kind of a discovered URL.
type Type string

const (
	TypePage    Type = "page"
	TypeAPI     Type = "api"
	TypeAsset   Type = "asset"
	TypeUnknown Type = "unknown"
)

// AssetKind refines TypeAsset.
type AssetKind string

const (
	AssetJavaScript AssetKind = "javascript"
	AssetStylesheet AssetKind = "stylesheet"
	AssetImage      AssetKind = "image"
	AssetFont       AssetKind = "font"
	AssetMedia      AssetKind = "media"
	AssetDocument   AssetKind = "document"
	AssetOther      AssetKind = "other"
)

// Input is everything known about a URL at classification time.
type Input struct {
	Path        string
	Hints       []discovery.Hint
	Status      int
	ContentType string
}

// Result is a classification with the evidence behind it.
type Result struct {
	Type      Type
	AssetKind AssetKind
	Evidence  string
}

var extKinds = map[string]AssetKind{
	".js": AssetJavaScript, ".mjs": AssetJavaScript, ".cjs": AssetJavaScript, ".map": AssetJavaScript,
	".css": AssetStylesheet,
	".png": AssetImage, ".jpg": AssetImage, ".jpeg": AssetImage, ".gif": AssetImage, ".svg": AssetImage,
	".webp": AssetImage, ".avif": AssetImage, ".ico": AssetImage, ".bmp": AssetImage,
	".woff": AssetFont, ".woff2": AssetFont, ".ttf": AssetFont, ".otf": AssetFont, ".eot": AssetFont,
	".mp4": AssetMedia, ".webm": AssetMedia, ".mp3": AssetMedia, ".wav": AssetMedia, ".ogg": AssetMedia, ".mov": AssetMedia,
	".pdf": AssetDocument, ".doc": AssetDocument, ".docx": AssetDocument, ".xls": AssetDocument,
	".xlsx": AssetDocument, ".ppt": AssetDocument, ".pptx": AssetDocument, ".csv": AssetDocument,
	".zip": AssetOther, ".gz": AssetOther, ".tar": AssetOther, ".dmg": AssetOther, ".exe": AssetOther,
	".txt": AssetOther, ".wasm": AssetOther,
}

var pageExts = map[string]bool{
	".html": true, ".htm": true, ".php": true, ".asp": true, ".aspx": true, ".jsp": true, ".shtml": true,
}

var hintKinds = map[discovery.Hint]AssetKind{
	discovery.HintScript:     AssetJavaScript,
	discovery.HintStylesheet: AssetStylesheet,
	discovery.HintImage:      AssetImage,
	discovery.HintFont:       AssetFont,
	discovery.HintMedia:      AssetMedia,
	discovery.HintManifest:   AssetOther,
}

// AssetKindForPath returns the asset kind implied by a path's file extension.
func AssetKindForPath(p string) (AssetKind, bool) {
	k, ok := extKinds[strings.ToLower(path.Ext(p))]
	return k, ok
}

var (
	apiSegment     = regexp.MustCompile(`(?i)(^|/)(api|apis|rest|graphql|gql|rpc|wp-json|oauth2?|v[0-9]+)(/|$)`)
	apiFileSuffix  = regexp.MustCompile(`(?i)\.(json|xml)$`)
	jsonMediaTypes = regexp.MustCompile(`(?i)^application/([a-z0-9.+-]*\+)?json$|^application/graphql`)
)

// LooksLikeAPIPath reports whether a path resembles an API route and returns
// a short description of the matched pattern.
func LooksLikeAPIPath(p string) (string, bool) {
	if m := apiSegment.FindStringSubmatch(p); m != nil {
		return "/" + strings.ToLower(m[2]) + "/", true
	}
	if apiFileSuffix.MatchString(p) && !strings.Contains(strings.ToLower(p), "sitemap") {
		return strings.ToLower(path.Ext(p)), true
	}
	return "", false
}

// Classify decides the type of a URL. Evidence is ordered from strongest
// (what the server actually returned) to weakest (what the path looks like).
func Classify(in Input) Result {
	hints := make(map[discovery.Hint]bool, len(in.Hints))
	for _, h := range in.Hints {
		hints[h] = true
	}

	// 1. A successful response's content type is the strongest signal.
	if in.Status >= 200 && in.Status < 300 && in.ContentType != "" {
		if r, ok := byContentType(in.ContentType); ok {
			return r
		}
	}

	// 2. How the URL was referenced (<script src>, <link rel=stylesheet>, ...).
	for _, h := range []discovery.Hint{
		discovery.HintScript, discovery.HintStylesheet, discovery.HintFont,
		discovery.HintImage, discovery.HintMedia, discovery.HintManifest,
	} {
		if hints[h] {
			return Result{Type: TypeAsset, AssetKind: hintKinds[h], Evidence: "Referenced as " + string(h) + " in HTML"}
		}
	}

	// 3. File extension.
	ext := strings.ToLower(path.Ext(in.Path))
	if k, ok := extKinds[ext]; ok {
		return Result{Type: TypeAsset, AssetKind: k, Evidence: "File extension " + ext}
	}

	// 4. API-like path. Explicitly unverified.
	if pattern, ok := LooksLikeAPIPath(in.Path); ok {
		return Result{Type: TypeAPI, Evidence: "Path contains " + pattern + " (not verified by a response)"}
	}

	if pageExts[ext] {
		return Result{Type: TypePage, Evidence: "File extension " + ext}
	}
	switch {
	case hints[discovery.HintEntry]:
		return Result{Type: TypePage, Evidence: "Scan start URL"}
	case hints[discovery.HintLink], hints[discovery.HintFrame]:
		return Result{Type: TypePage, Evidence: "Linked from HTML"}
	case hints[discovery.HintRedirect]:
		return Result{Type: TypePage, Evidence: "Target of a redirect"}
	}
	if hints[discovery.HintForm] {
		return Result{Type: TypeUnknown, Evidence: "Form submission target (not requested)"}
	}
	return Result{Type: TypeUnknown, Evidence: "Not enough information to classify"}
}

func byContentType(ct string) (Result, bool) {
	ct = strings.ToLower(ct)
	ev := "Responded with " + ct
	switch {
	case ct == "text/html" || ct == "application/xhtml+xml":
		return Result{Type: TypePage, Evidence: ev}, true
	case jsonMediaTypes.MatchString(ct):
		return Result{Type: TypeAPI, Evidence: ev}, true
	case strings.Contains(ct, "javascript") || ct == "text/ecmascript":
		return Result{Type: TypeAsset, AssetKind: AssetJavaScript, Evidence: ev}, true
	case ct == "text/css":
		return Result{Type: TypeAsset, AssetKind: AssetStylesheet, Evidence: ev}, true
	case strings.HasPrefix(ct, "image/"):
		return Result{Type: TypeAsset, AssetKind: AssetImage, Evidence: ev}, true
	case strings.HasPrefix(ct, "font/") || strings.Contains(ct, "font"):
		return Result{Type: TypeAsset, AssetKind: AssetFont, Evidence: ev}, true
	case strings.HasPrefix(ct, "video/") || strings.HasPrefix(ct, "audio/"):
		return Result{Type: TypeAsset, AssetKind: AssetMedia, Evidence: ev}, true
	case ct == "application/pdf":
		return Result{Type: TypeAsset, AssetKind: AssetDocument, Evidence: ev}, true
	}
	return Result{}, false
}
