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
	ID          string `json:"id"`
	ImageURL    string `json:"image_url"`
	Title       string `json:"title"`
	Description string `json:"description"`
	PinnerName  string `json:"pinner_name"`
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

	router.GET("/", renderPage("index.html"))
	router.GET("/search/pins/", searchHandler)
	router.GET("/pin/:id", pinHandler)
	router.GET("/image", proxyImageHandler)
	router.GET("/about", renderPage("about.html"))
	router.GET("/licenses", renderPage("licenses.html"))

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

func searchHandler(c *gin.Context) {
	query := strings.TrimSpace(c.Query("q"))

	bookmark := cookieValue(c, searchBookmarkKey)

	if _, nextExists := c.GetQuery("next"); !nextExists {
		clearCookie(c, searchBookmarkKey)
		bookmark = ""
	}

	csrftoken := cookieValue(c, "csrftoken")

	options := map[string]any{
		"query": query,
	}
	if bookmark != "" {
		options["bookmarks"] = []string{bookmark}
	}
	dataParam, err := marshalPinterestOptions(options)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to encode data"})
		return
	}

	method := http.MethodGet
	queryValues := url.Values{"data": {dataParam}}
	var formValues url.Values
	if bookmark != "" {
		method = http.MethodPost
		queryValues = nil
		formValues = url.Values{"data": {dataParam}}
	}

	resp, bodyBytes, err := doPinterestRequest(
		c.Request.Context(),
		method,
		pinterestSearchURL,
		queryValues,
		formValues,
		"www/search/[scope].js",
		"",
		csrftoken,
	)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Request failed"})
		return
	}

	if csrfToken := responseCookieValue(resp, "csrftoken"); csrfToken != "" {
		c.SetCookie("csrftoken", csrfToken, 0, "/", "", false, true)
	}
	if resp.StatusCode != http.StatusOK {
		renderSearchError(c, query, resp, "Upstream error", bodyBytes, nil)
		return
	}

	var responseData struct {
		ResourceResponse struct {
			Data struct {
				Results []struct {
					ID     string `json:"id"`
					Images struct {
						Orig struct {
							URL string `json:"url"`
						} `json:"orig"`
					} `json:"images"`
				} `json:"results"`
			} `json:"data"`
			Bookmark string `json:"bookmark,omitempty"`
		} `json:"resource_response"`
	}

	if err := json.Unmarshal(bodyBytes, &responseData); err != nil {
		renderSearchError(c, query, resp, "Failed to decode response", bodyBytes, err)
		return
	}

	if responseData.ResourceResponse.Bookmark != "" {
		c.SetCookie(searchBookmarkKey, responseData.ResourceResponse.Bookmark, 0, "/", "", false, true)
	} else {
		clearCookie(c, searchBookmarkKey)
	}

	var results []ResultItem
	for _, result := range responseData.ResourceResponse.Data.Results {
		imageURL := result.Images.Orig.URL
		if imageURL != "" && isAllowedDomain(imageURL) {
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
	})
}

func pinHandler(c *gin.Context) {
	pinID := c.Param("id")
	query := c.Query("q")
	from := c.Query("from")

	bookmark := cookieValue(c, relatedBookmarkKey)

	if _, nextExists := c.GetQuery("next"); !nextExists {
		clearCookie(c, relatedBookmarkKey)
		bookmark = ""
	}

	csrftoken := cookieValue(c, "csrftoken")

	pin := fetchPinDetails(c.Request.Context(), pinID, csrftoken)

	related, nextBookmark := fetchRelatedPins(c.Request.Context(), pinID, csrftoken, bookmark)

	if nextBookmark != "" {
		c.SetCookie(relatedBookmarkKey, nextBookmark, 0, "/", "", false, true)
	} else {
		clearCookie(c, relatedBookmarkKey)
	}

	c.HTML(http.StatusOK, "pin.html", gin.H{
		"Pin":             pin,
		"Related":         related,
		"RelatedBookmark": nextBookmark,
		"Query":           query,
		"From":            from,
	})
}

func fetchPinDetails(ctx context.Context, pinID string, csrftoken string) Pin {
	sourceURL := fmt.Sprintf("/pin/%s/", pinID)
	options := map[string]any{
		"id": pinID,
	}
	dataParam, err := marshalPinterestOptions(options)
	if err != nil {
		return Pin{ID: pinID}
	}

	resp, bodyBytes, err := doPinterestRequest(
		ctx,
		http.MethodGet,
		pinterestPinURL,
		url.Values{
			"source_url": {sourceURL},
			"data":       {dataParam},
		},
		nil,
		fmt.Sprintf("www/pin/%s.js", pinID),
		sourceURL,
		csrftoken,
	)
	if err != nil {
		return Pin{ID: pinID}
	}
	if resp.StatusCode != http.StatusOK {
		return Pin{ID: pinID}
	}

	var singlePinResponse struct {
		ResourceResponse struct {
			Data struct {
				Title       string `json:"title"`
				Description string `json:"description"`
				Images      struct {
					Orig struct {
						URL string `json:"url"`
					} `json:"orig"`
					Size736x struct {
						URL string `json:"url"`
					} `json:"736x"`
					Size474x struct {
						URL string `json:"url"`
					} `json:"474x"`
					Size564x struct {
						URL string `json:"url"`
					} `json:"564x"`
					Size236x struct {
						URL string `json:"url"`
					} `json:"236x"`
				} `json:"images"`
				Pinner struct {
					FullName string `json:"full_name"`
				} `json:"pinner"`
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"data"`
		} `json:"resource_response"`
	}

	if err := json.Unmarshal(bodyBytes, &singlePinResponse); err != nil {
		return Pin{ID: pinID}
	}

	data := singlePinResponse.ResourceResponse.Data

	pin := Pin{
		ID:          pinID,
		Title:       strings.TrimSpace(data.Title),
		Description: strings.TrimSpace(data.Description),
		PinnerName:  strings.TrimSpace(data.Pinner.FullName),
	}

	pin.ImageURL = proxiedImageURL(firstNonEmpty(
		data.Images.Orig.URL,
		data.Images.Size736x.URL,
		data.Images.Size564x.URL,
		data.Images.Size474x.URL,
		data.Images.Size236x.URL,
	))

	return pin
}

func fetchRelatedPins(ctx context.Context, pinID string, csrftoken string, bookmark string) ([]Pin, string) {
	sourceURL := fmt.Sprintf("/pin/%s/", pinID)
	options := map[string]any{
		"pin_id":    pinID,
		"page_size": 12,
		"source":    "pin",
	}
	if bookmark != "" {
		options["bookmarks"] = []string{bookmark}
	}
	dataParam, err := marshalPinterestOptions(options)
	if err != nil {
		return []Pin{}, ""
	}

	method := http.MethodGet
	queryValues := url.Values{
		"source_url": {sourceURL},
		"data":       {dataParam},
	}
	var formValues url.Values
	if bookmark != "" {
		method = http.MethodPost
		queryValues = nil
		formValues = url.Values{"data": {dataParam}}
	}

	resp, bodyBytes, err := doPinterestRequest(
		ctx,
		method,
		pinterestRelatedURL,
		queryValues,
		formValues,
		fmt.Sprintf("www/pin/%s.js", pinID),
		sourceURL,
		csrftoken,
	)
	if err != nil {
		return []Pin{}, ""
	}
	if resp.StatusCode != http.StatusOK {
		return []Pin{}, ""
	}

	var responseData struct {
		ResourceResponse struct {
			Data []struct {
				ID          string          `json:"id"`
				Type        string          `json:"type"`
				StoryType   string          `json:"story_type"`
				TitleRaw    json.RawMessage `json:"title"`
				GridTitle   string          `json:"grid_title"`
				Description string          `json:"description"`
				Images      struct {
					Orig struct {
						URL string `json:"url"`
					} `json:"orig"`
					Size474x struct {
						URL string `json:"url"`
					} `json:"474x"`
					Size736x struct {
						URL string `json:"url"`
					} `json:"736x"`
					Size564x struct {
						URL string `json:"url"`
					} `json:"564x"`
					Size236x struct {
						URL string `json:"url"`
					} `json:"236x"`
				} `json:"images"`
				Pinner struct {
					FullName string `json:"full_name"`
				} `json:"pinner"`
				AggregatedPinData struct {
					AggregatedStats struct {
						Saves int `json:"saves"`
					} `json:"aggregated_stats"`
				} `json:"aggregated_pin_data"`
			} `json:"data"`
			Bookmark string `json:"bookmark,omitempty"`
		} `json:"resource_response"`
	}

	if err := json.Unmarshal(bodyBytes, &responseData); err != nil {
		return []Pin{}, ""
	}

	var related []Pin
	for _, result := range responseData.ResourceResponse.Data {
		if result.Type != "pin" || result.ID == "" {
			continue
		}

		title := ""
		if len(result.TitleRaw) > 0 {
			var titleObj struct {
				Format string   `json:"format"`
				Args   []string `json:"args"`
			}
			if err := json.Unmarshal(result.TitleRaw, &titleObj); err == nil && titleObj.Format != "" {
				title = titleObj.Format
			} else {
				var titleStr string
				if err := json.Unmarshal(result.TitleRaw, &titleStr); err == nil {
					title = titleStr
				}
			}
		}
		if title == "" {
			title = result.GridTitle
		}
		if title == "" {
			title = result.Description
		}

		imageURL := proxiedImageURL(firstNonEmpty(
			result.Images.Orig.URL,
			result.Images.Size736x.URL,
			result.Images.Size564x.URL,
			result.Images.Size474x.URL,
			result.Images.Size236x.URL,
		))

		if imageURL != "" {
			related = append(related, Pin{
				ID:         result.ID,
				ImageURL:   imageURL,
				Title:      strings.TrimSpace(title),
				PinnerName: strings.TrimSpace(result.Pinner.FullName),
			})
		}
	}

	return related, responseData.ResourceResponse.Bookmark
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
	return func(c *gin.Context) {
		c.HTML(http.StatusOK, name, nil)
	}
}

func mustSubFS(fsys fs.FS, dir string) fs.FS {
	subFS, err := fs.Sub(fsys, dir)
	if err != nil {
		log.Fatalf("load embedded filesystem %q: %v", dir, err)
	}
	return subFS
}

func renderSearchError(c *gin.Context, query string, resp *http.Response, message string, body []byte, decodeErr error) {
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
