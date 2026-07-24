package cogneemock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// RequestLog records a single captured HTTP request.
type RequestLog struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
	Time    time.Time           `json:"time"`
}

// captureMiddleware reads and stores the request body, then dispatches to the next handler.
// The body is parsed based on Content-Type to extract meaningful fields.
func (s *Server) captureMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Read the full body
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			bodyBytes = nil
		}
		r.Body.Close()

		// Parse body based on Content-Type
		parsedBody := s.parseBody(r, bodyBytes)

		entry := RequestLog{
			Method:  r.Method,
			Path:    r.URL.Path,
			Headers: r.Header,
			Body:    parsedBody,
			Time:    time.Now().UTC(),
		}

		s.mu.Lock()
		s.requests = append(s.requests, entry)
		s.mu.Unlock()

		// Restore body for handlers that need it (they re-read from a new reader)
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		next(w, r)
	}
}

// parseBody extracts meaningful content from the request body based on Content-Type.
func (s *Server) parseBody(r *http.Request, body []byte) string {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		ct = http.DetectContentType(body)
	}

	mediaType, _, _ := mime.ParseMediaType(ct)

	switch {
	case strings.HasPrefix(mediaType, "multipart/form-data"):
		return parseMultipartBody(r.Header.Get("Content-Type"), body)
	case strings.HasPrefix(mediaType, "application/json"):
		return string(body)
	default:
		return string(body)
	}
}

// parseMultipartBody parses multipart form data and returns a formatted string
// containing the field values. For the /remember endpoint, this extracts
// datasetName and data fields.
func parseMultipartBoundary(contentType string, body []byte) string {
	return parseMultipartBody(contentType, body)
}

func parseMultipartBody(contentType string, body []byte) string {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return string(body)
	}

	boundary, ok := params["boundary"]
	if !ok {
		return string(body)
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var parts []string

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}

		fieldName := part.FormName()
		if fieldName == "" {
			fieldName = part.FileName()
		}

		data, err := io.ReadAll(part)
		part.Close()
		if err != nil {
			continue
		}

		parts = append(parts, fmt.Sprintf("%s=%s", fieldName, string(data)))
	}

	return strings.Join(parts, " ")
}

// parseJSONBody is a helper that unmarshals JSON body into a map.
// Returns nil if parsing fails.
func parseJSONBody(body []byte) map[string]interface{} {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil
	}
	return m
}
