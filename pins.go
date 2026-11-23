package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"unicode"

	"github.com/gin-gonic/gin"
)

// Pinterest's PinResource / RelatedModulesResource endpoints no longer resolve
// the opaque "e1_..." IDs that search returns, so pin details are remembered
// from the search results that surfaced them, and related pins fall back to a
// search for the pin's keywords.

const (
	pinStoreLimit        = 5000
	searchFallbackPrefix = "search:"
	pinLoadErrorMessage  = "Couldn't load this pin. Pinterest didn't return its details, so try opening it from the search results."
)

type pinImage struct {
	URL string `json:"url"`
}

// pinPayload is the subset of a Pinterest pin object shared by the search,
// pin and related-pins responses.
type pinPayload struct {
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	TitleRaw    json.RawMessage `json:"title"`
	GridTitle   string          `json:"grid_title"`
	Description string          `json:"description"`
	Images      struct {
		Orig     pinImage `json:"orig"`
		Size736x pinImage `json:"736x"`
		Size564x pinImage `json:"564x"`
		Size474x pinImage `json:"474x"`
		Size236x pinImage `json:"236x"`
	} `json:"images"`
	Pinner struct {
		FullName string `json:"full_name"`
	} `json:"pinner"`
	PinJoin struct {
		VisualAnnotation []string `json:"visual_annotation"`
	} `json:"pin_join"`
}

func (p pinPayload) title() string {
	if len(p.TitleRaw) > 0 {
		var titleObj struct {
			Format string `json:"format"`
		}
		if err := json.Unmarshal(p.TitleRaw, &titleObj); err == nil && titleObj.Format != "" {
			return strings.TrimSpace(titleObj.Format)
		}
		var titleStr string
		if err := json.Unmarshal(p.TitleRaw, &titleStr); err == nil && strings.TrimSpace(titleStr) != "" {
			return strings.TrimSpace(titleStr)
		}
	}
	return strings.TrimSpace(p.GridTitle)
}

func (p pinPayload) imageURL() string {
	return proxiedImageURL(firstNonEmpty(
		p.Images.Orig.URL,
		p.Images.Size736x.URL,
		p.Images.Size564x.URL,
		p.Images.Size474x.URL,
		p.Images.Size236x.URL,
	))
}

func (p pinPayload) toPin() Pin {
	return Pin{
		ID:          p.ID,
		ImageURL:    p.imageURL(),
		Title:       p.title(),
		Description: strings.TrimSpace(p.Description),
		PinnerName:  strings.TrimSpace(p.Pinner.FullName),
		Keywords:    p.PinJoin.VisualAnnotation,
	}
}

// pinStore is a bounded in-memory map of pins seen in upstream responses. The
// oldest entry is evicted first once the limit is reached.
type pinStore struct {
	mu   sync.RWMutex
	pins map[string]Pin
	ring []string
	next int
}

var seenPins = &pinStore{pins: make(map[string]Pin)}

func (s *pinStore) get(id string) (Pin, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pin, ok := s.pins[id]
	return pin, ok
}

func (s *pinStore) put(pin Pin) {
	if pin.ID == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if old, ok := s.pins[pin.ID]; ok {
		// Keep fields a thinner response (e.g. related pins) didn't carry.
		pin.ImageURL = firstNonEmpty(pin.ImageURL, old.ImageURL)
		pin.Title = firstNonEmpty(pin.Title, old.Title)
		pin.Description = firstNonEmpty(pin.Description, old.Description)
		pin.PinnerName = firstNonEmpty(pin.PinnerName, old.PinnerName)
		if len(pin.Keywords) == 0 {
			pin.Keywords = old.Keywords
		}
	} else if len(s.ring) < pinStoreLimit {
		s.ring = append(s.ring, pin.ID)
	} else {
		delete(s.pins, s.ring[s.next])
		s.ring[s.next] = pin.ID
		s.next = (s.next + 1) % pinStoreLimit
	}
	s.pins[pin.ID] = pin
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
	ctx := c.Request.Context()

	hideAI := hideAIPreference(c)

	pin, ok := loadPin(ctx, pinID, csrftoken)
	if !ok {
		clearCookie(c, relatedBookmarkKey)
		c.HTML(http.StatusBadGateway, "pin.html", gin.H{
			"Pin":    Pin{ID: pinID},
			"Query":  query,
			"From":   from,
			"HideAI": hideAI,
			"Error":  pinLoadErrorMessage,
		})
		return
	}

	related, nextBookmark := loadRelatedPins(ctx, pin, query, hideAI, csrftoken, bookmark)

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
		"HideAI":          hideAI,
	})
}

// loadPin returns a pin remembered from earlier search results, or asks
// Pinterest for it. The bool is false when neither source knows the pin.
func loadPin(ctx context.Context, pinID string, csrftoken string) (Pin, bool) {
	if pin, ok := seenPins.get(pinID); ok {
		return pin, true
	}

	pin, ok := fetchPinDetails(ctx, pinID, csrftoken)
	if ok {
		seenPins.put(pin)
	}
	return pin, ok
}

func fetchPinDetails(ctx context.Context, pinID string, csrftoken string) (Pin, bool) {
	sourceURL := "/pin/" + pinID + "/"
	dataParam, err := marshalPinterestOptions(map[string]any{"id": pinID})
	if err != nil {
		return Pin{}, false
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
		"www/pin/"+pinID+".js",
		sourceURL,
		csrftoken,
	)
	if err != nil || resp.StatusCode != http.StatusOK {
		return Pin{}, false
	}

	var singlePinResponse struct {
		ResourceResponse struct {
			Data pinPayload `json:"data"`
		} `json:"resource_response"`
	}
	if err := json.Unmarshal(bodyBytes, &singlePinResponse); err != nil {
		return Pin{}, false
	}

	pin := singlePinResponse.ResourceResponse.Data.toPin()
	pin.ID = pinID
	if pin.ImageURL == "" && pin.Title == "" && pin.Description == "" {
		return Pin{}, false
	}
	return pin, true
}

// loadRelatedPins asks Pinterest for related pins and, when it has none for a
// pin, falls back to searching for the pin's keywords. Bookmarks from the
// fallback are tagged with searchFallbackPrefix so the next page continues it.
// searchQuery is the term the user searched for, used when the pin itself has
// no keywords, title or description to search by.
func loadRelatedPins(ctx context.Context, pin Pin, searchQuery string, hideAI bool, csrftoken string, bookmark string) ([]Pin, string) {
	if searchBookmark, ok := strings.CutPrefix(bookmark, searchFallbackPrefix); ok {
		return fetchSimilarPins(ctx, pin, searchQuery, hideAI, csrftoken, searchBookmark)
	}

	related, next := fetchRelatedPins(ctx, pin.ID, hideAI, csrftoken, bookmark)
	if len(related) > 0 || bookmark != "" {
		return related, next
	}
	return fetchSimilarPins(ctx, pin, searchQuery, hideAI, csrftoken, "")
}

func fetchRelatedPins(ctx context.Context, pinID string, hideAI bool, csrftoken string, bookmark string) ([]Pin, string) {
	sourceURL := "/pin/" + pinID + "/"
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
		return nil, ""
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
		"www/pin/"+pinID+".js",
		sourceURL,
		csrftoken,
	)
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil, ""
	}

	var responseData struct {
		ResourceResponse struct {
			Data     []pinPayload `json:"data"`
			Bookmark string       `json:"bookmark,omitempty"`
		} `json:"resource_response"`
	}
	if err := json.Unmarshal(bodyBytes, &responseData); err != nil {
		return nil, ""
	}

	related := pinsFromPayloads(responseData.ResourceResponse.Data, pinID, hideAI)
	return related, responseData.ResourceResponse.Bookmark
}

// fetchSimilarPins searches Pinterest for the pin's keywords. The returned
// bookmark is already tagged with searchFallbackPrefix.
func fetchSimilarPins(ctx context.Context, pin Pin, searchQuery string, hideAI bool, csrftoken string, bookmark string) ([]Pin, string) {
	query := similarQuery(pin, searchQuery)
	if query == "" {
		return nil, ""
	}

	resp, bodyBytes, err := searchPinterest(ctx, query, csrftoken, bookmark)
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil, ""
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
		return nil, ""
	}

	related := pinsFromPayloads(responseData.ResourceResponse.Data.Results, pin.ID, hideAI)
	next := responseData.ResourceResponse.Bookmark
	if next != "" {
		next = searchFallbackPrefix + next
	}
	return related, next
}

func similarQuery(pin Pin, searchQuery string) string {
	if len(pin.Keywords) > 0 {
		return pin.Keywords[0]
	}
	if usableQuery(pin.Title) {
		return pin.Title
	}
	if description := strings.Join(strings.Fields(pin.Description), " "); usableQuery(description) {
		if runes := []rune(description); len(runes) > 80 {
			description = string(runes[:80])
		}
		return description
	}
	return strings.TrimSpace(searchQuery)
}

// usableQuery reports whether text has enough real words to search for, which
// rules out titles that are only emoji or punctuation.
func usableQuery(text string) bool {
	count := 0
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			count++
		}
	}
	return count >= 3
}

// pinsFromPayloads converts upstream pins that have an image, remembering each
// so its detail page can be opened later. Pins that look AI-generated are
// dropped when hideAI is set.
func pinsFromPayloads(payloads []pinPayload, excludeID string, hideAI bool) []Pin {
	var pins []Pin
	for _, payload := range payloads {
		if payload.ID == "" || payload.ID == excludeID {
			continue
		}
		if payload.Type != "" && payload.Type != "pin" {
			continue
		}
		if hideAI && payload.looksAIGenerated() {
			continue
		}
		pin := payload.toPin()
		if pin.ImageURL == "" {
			continue
		}
		seenPins.put(pin)
		pins = append(pins, pin)
	}
	return pins
}
