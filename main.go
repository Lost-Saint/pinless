package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

//go:embed templates/*
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

type Pin struct {
	ID          string   `json:"id"`
	ImageURL    string   `json:"image_url"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	PinnerName  string   `json:"pinner_name"`
	Keywords    []string `json:"-"`
}

type ResultItem struct {
	ID    string
	Image string
}

const (
	listenAddr          = ":3000"
	pinterestSearchURL  = "https://www.pinterest.com/resource/BaseSearchResource/get/"
	pinterestPinURL     = "https://www.pinterest.com/resource/PinResource/get/"
	pinterestRelatedURL = "https://www.pinterest.com/resource/RelatedModulesResource/get/"
	upstreamUserAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"
	searchBookmarkKey   = "search_bookmark"
	relatedBookmarkKey  = "related_bookmark"
	upstreamAttempts    = 3
	upstreamRetryDelay  = 150 * time.Millisecond
)

var (
	allowedDomains = []string{"pinimg.com", "pinterest.com"}
	upstreamClient = &http.Client{Timeout: 20 * time.Second}
)

func main() {
	gin.SetMode(gin.ReleaseMode)

	router := gin.New()
	router.Use(gin.Recovery())

	router.StaticFS("/static", http.FS(mustSubFS(staticFS, "static")))
	router.SetHTMLTemplate(template.Must(template.ParseFS(templatesFS, "templates/*")))

	router.GET("/", indexHandler)
	router.GET("/search/pins/", searchHandler)
	router.GET("/pin/:id", pinHandler)
	router.GET("/image", proxyImageHandler)
	router.GET("/about", renderPage("about.html"))
	router.GET("/licenses", renderPage("licenses.html"))
	router.NoRoute(renderPageWithStatus("404.html", http.StatusNotFound))

	fmt.Print(` _____ _     _             
|  _  |_|___| |___ ___ ___ 
|   __| |   | | -_|_ -|_ -|
|__|  |_|_|_|_|___|___|___|
`)
	log.Printf("server running at http://0.0.0.0%s", listenAddr)

	if err := router.Run(listenAddr); err != nil {
		log.Fatal(err)
	}
}

func indexHandler(c *gin.Context) {
	c.HTML(http.StatusOK, "index.html", gin.H{"HideAI": hideAIPreference(c)})
}

func searchHandler(c *gin.Context) {
	query := strings.TrimSpace(c.Query("q"))
	hideAI := hideAIPreference(c)

	bookmark := cookieValue(c, searchBookmarkKey)

	if _, nextExists := c.GetQuery("next"); !nextExists {
		clearCookie(c, searchBookmarkKey)
		bookmark = ""
	}

	csrftoken := cookieValue(c, "csrftoken")

	resp, bodyBytes, err := searchPinterest(c.Request.Context(), query, csrftoken, bookmark)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Request failed"})
		return
	}

	if csrfToken := responseCookieValue(resp, "csrftoken"); csrfToken != "" {
		c.SetCookie("csrftoken", csrfToken, 0, "/", "", false, true)
	}
	if resp.StatusCode != http.StatusOK {
		renderSearchError(c, query, hideAI, resp, "Upstream error", bodyBytes, nil)
		return
	}

	var responseData struct {
		ResourceResponse struct {
			Data struct {
				Results []pinPayload `json:"results"`
			} `json:"data"`
			Bookmark string `json:"bookmark,omitempty"`
		} `json:"resource_response"`
	}

	if err := json.Unmarshal(bodyBytes, &responseData); err != nil {
		renderSearchError(c, query, hideAI, resp, "Failed to decode response", bodyBytes, err)
		return
	}

	if responseData.ResourceResponse.Bookmark != "" {
		c.SetCookie(searchBookmarkKey, responseData.ResourceResponse.Bookmark, 0, "/", "", false, true)
	} else {
		clearCookie(c, searchBookmarkKey)
	}

	var results []ResultItem
	hiddenAI := 0
	for _, result := range responseData.ResourceResponse.Data.Results {
		imageURL := result.Images.Orig.URL
		if imageURL != "" && isAllowedDomain(imageURL) {
			if hideAI && result.looksAIGenerated() {
				hiddenAI++
				continue
			}
			seenPins.put(result.toPin())
			results = append(results, ResultItem{
				ID:    result.ID,
				Image: proxiedImageURL(imageURL),
			})
		}
	}

	c.HTML(http.StatusOK, "results.html", gin.H{
		"Results":  results,
		"Bookmark": responseData.ResourceResponse.Bookmark,
		"Query":    query,
		"HideAI":   hideAI,
		"HiddenAI": hiddenAI,
	})
}

// searchPinterest runs a pin search, continuing from bookmark when it is set.
func searchPinterest(ctx context.Context, query string, csrftoken string, bookmark string) (*http.Response, []byte, error) {
	options := map[string]any{
		"query": query,
	}
	if bookmark != "" {
		options["bookmarks"] = []string{bookmark}
	}
	dataParam, err := marshalPinterestOptions(options)
	if err != nil {
		return nil, nil, err
	}

	method := http.MethodGet
	queryValues := url.Values{"data": {dataParam}}
	var formValues url.Values
	if bookmark != "" {
		method = http.MethodPost
		queryValues = nil
		formValues = url.Values{"data": {dataParam}}
	}

	return doPinterestRequest(
		ctx,
		method,
		pinterestSearchURL,
		queryValues,
		formValues,
		"www/search/[scope].js",
		"",
		csrftoken,
	)
}

func proxyImageHandler(c *gin.Context) {
	imageURL := c.Query("url")
	if !isAllowedDomain(imageURL) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Domain not allowed"})
		return
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, imageURL, nil)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid image URL"})
		return
	}
	req.Header.Set("User-Agent", upstreamUserAgent)

	resp, err := upstreamClient.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Failed to fetch image"})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Image upstream returned non-200"})
		return
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	headers := map[string]string{}
	for _, header := range []string{"Cache-Control", "ETag", "Last-Modified"} {
		if value := resp.Header.Get(header); value != "" {
			headers[header] = value
		}
	}

	c.DataFromReader(http.StatusOK, resp.ContentLength, contentType, resp.Body, headers)
}

func isAllowedDomain(urlStr string) bool {
	parsedURL, err := url.Parse(urlStr)
	if err != nil {
		return false
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return false
	}

	host := strings.ToLower(parsedURL.Hostname())
	if host == "" {
		return false
	}

	return slices.ContainsFunc(allowedDomains, func(domain string) bool {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
		return false
	})
}

// doPinterestRequest calls Pinterest, retrying a few times when it answers 403.
// Pinterest intermittently rejects otherwise valid requests with 403 (code
// 6427) and accepts the same request when it is repeated right away.
func doPinterestRequest(
	ctx context.Context,
	method string,
	endpoint string,
	queryValues url.Values,
	formValues url.Values,
	handler string,
	sourceURL string,
	csrftoken string,
) (*http.Response, []byte, error) {
	var (
		resp      *http.Response
		bodyBytes []byte
		err       error
	)
	for attempt := 0; attempt < upstreamAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(upstreamRetryDelay):
			}
		}

		resp, bodyBytes, err = sendPinterestRequest(ctx, method, endpoint, queryValues, formValues, handler, sourceURL, csrftoken)
		if err != nil || resp.StatusCode != http.StatusForbidden {
			break
		}
	}
	return resp, bodyBytes, err
}

func sendPinterestRequest(
	ctx context.Context,
	method string,
	endpoint string,
	queryValues url.Values,
	formValues url.Values,
	handler string,
	sourceURL string,
	csrftoken string,
) (*http.Response, []byte, error) {
	var body io.Reader
	if method == http.MethodPost && len(formValues) > 0 {
		body = strings.NewReader(formValues.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, nil, err
	}
	if len(queryValues) > 0 {
		req.URL.RawQuery = queryValues.Encode()
	}
	setPinterestHeaders(req, handler, sourceURL, csrftoken)
	if method == http.MethodPost && len(formValues) > 0 {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := upstreamClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}

	return resp, bodyBytes, nil
}

func setPinterestHeaders(req *http.Request, handler string, sourceURL string, csrftoken string) {
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("User-Agent", upstreamUserAgent)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("x-pinterest-pws-handler", handler)
	if sourceURL != "" {
		req.Header.Set("x-pinterest-source-url", sourceURL)
		req.Header.Set("Referer", "https://www.pinterest.com/")
	}
	if csrftoken != "" {
		req.Header.Set("x-csrftoken", csrftoken)
		req.Header.Set("Cookie", (&http.Cookie{Name: "csrftoken", Value: csrftoken}).String())
	}
}

func marshalPinterestOptions(options map[string]any) (string, error) {
	body, err := json.Marshal(map[string]any{"options": options})
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func responseCookieValue(resp *http.Response, name string) string {
	for _, cookie := range resp.Cookies() {
		if cookie != nil && cookie.Name == name && cookie.Value != "" {
			return cookie.Value
		}
	}
	return ""
}

func clearCookie(c *gin.Context, name string) {
	c.SetCookie(name, "", -1, "/", "", false, true)
}

func cookieValue(c *gin.Context, name string) string {
	cookie, err := c.Cookie(name)
	if err != nil {
		return ""
	}
	return cookie
}

func renderPage(name string) gin.HandlerFunc {
	return renderPageWithStatus(name, http.StatusOK)
}

func renderPageWithStatus(name string, status int) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.HTML(status, name, nil)
	}
}

func mustSubFS(fsys fs.FS, dir string) fs.FS {
	subFS, err := fs.Sub(fsys, dir)
	if err != nil {
		log.Fatalf("load embedded filesystem %q: %v", dir, err)
	}
	return subFS
}

func renderSearchError(c *gin.Context, query string, hideAI bool, resp *http.Response, message string, body []byte, decodeErr error) {
	errorData := gin.H{
		"error": message,
	}
	if resp != nil {
		errorData["upstream_status"] = resp.Status
		errorData["content_type"] = resp.Header.Get("Content-Type")
	}
	if decodeErr != nil {
		errorData["decode_error"] = decodeErr.Error()
	}
	if len(body) > 0 {
		errorData["body"] = truncatedBody(body, 500)
	}

	c.HTML(http.StatusBadGateway, "results.html", gin.H{
		"Results": nil,
		"Query":   query,
		"HideAI":  hideAI,
		"Error":   errorData,
	})
}

func truncatedBody(body []byte, limit int) string {
	if len(body) <= limit {
		return string(body)
	}
	return string(body[:limit])
}

func proxiedImageURL(imageURL string) string {
	if imageURL == "" {
		return ""
	}
	return fmt.Sprintf("/image?url=%s", url.QueryEscape(imageURL))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
