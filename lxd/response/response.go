package response

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"time"

	log "gopkg.in/inconshreveable/log15.v2"

	"github.com/lxc/lxd/client"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/logging"
)

var debug bool

// Init sets the debug variable to the provided value.
func Init(d bool) {
	debug = d
}

// Response represents an API response
type Response interface {
	Render(w http.ResponseWriter) error
	String() string
}

// Sync response
type syncResponse struct {
	success   bool
	etag      interface{}
	metadata  interface{}
	location  string
	code      int
	headers   map[string]string
	plaintext bool
}

// EmptySyncResponse represents an empty syncResponse.
var EmptySyncResponse = &syncResponse{success: true, metadata: make(map[string]interface{})}

// SyncResponse returns a new syncResponse with the success and metadata fields
// set to the provided values.
func SyncResponse(success bool, metadata interface{}) Response {
	return &syncResponse{success: success, metadata: metadata}
}

// SyncResponseETag returns a new syncResponse with an etag.
func SyncResponseETag(success bool, metadata interface{}, etag interface{}) Response {
	return &syncResponse{success: success, metadata: metadata, etag: etag}
}

// SyncResponseLocation returns a new syncResponse with a location.
func SyncResponseLocation(success bool, metadata interface{}, location string) Response {
	return &syncResponse{success: success, metadata: metadata, location: location}
}

// SyncResponseRedirect returns a new syncResponse with a location, indicating
// a permanent redirect.
func SyncResponseRedirect(address string) Response {
	return &syncResponse{success: true, location: address, code: http.StatusPermanentRedirect}
}

// SyncResponseHeaders returns a new syncResponse with headers.
func SyncResponseHeaders(success bool, metadata interface{}, headers map[string]string) Response {
	return &syncResponse{success: success, metadata: metadata, headers: headers}
}

// SyncResponsePlain return a new syncResponse with plaintext.
func SyncResponsePlain(success bool, metadata string) Response {
	return &syncResponse{success: success, metadata: metadata, plaintext: true}
}

func (r *syncResponse) Render(w http.ResponseWriter) error {
	// Set an appropriate ETag header
	if r.etag != nil {
		etag, err := util.EtagHash(r.etag)
		if err == nil {
			w.Header().Set("ETag", fmt.Sprintf("\"%s\"", etag))
		}
	}

	// Prepare the JSON response
	status := api.Success
	if !r.success {
		status = api.Failure
	}

	if r.headers != nil {
		for h, v := range r.headers {
			w.Header().Set(h, v)
		}
	}

	code := r.code

	if r.location != "" {
		w.Header().Set("Location", r.location)
		if code == 0 {
			code = 201
		}
	}

	// Handle plain text headers.
	if r.plaintext {
		w.Header().Set("Content-Type", "text/plain")
	}

	// Write header and status code.
	if code == 0 {
		code = http.StatusOK
	}

	w.WriteHeader(code)

	// Handle plain text responses.
	if r.plaintext {
		if r.metadata != nil {
			_, err := w.Write([]byte(r.metadata.(string)))
			if err != nil {
				return err
			}
		}

		return nil
	}

	// Handle JSON responses.
	resp := api.ResponseRaw{
		Type:       api.SyncResponse,
		Status:     status.String(),
		StatusCode: int(status),
		Metadata:   r.metadata,
	}

	var debugLogger logger.Logger
	if debug {
		debugLogger = logging.AddContext(logger.Log, log.Ctx{"http_code": code})
	}

	return util.WriteJSON(w, resp, debugLogger)
}

func (r *syncResponse) String() string {
	if r.success {
		return "success"
	}

	return "failure"
}

// Error response
type errorResponse struct {
	code int    // Code to return in both the HTTP header and Code field of the response body.
	msg  string // Message to return in the Error field of the response body.
}

// ErrorResponse returns an error response with the given code and msg.
func ErrorResponse(code int, msg string) Response {
	return &errorResponse{code, msg}
}

// BadRequest returns a bad request response (400) with the given error.
func BadRequest(err error) Response {
	return &errorResponse{http.StatusBadRequest, err.Error()}
}

// Conflict returns a conflict response (409) with the given error.
func Conflict(err error) Response {
	message := "already exists"
	if err != nil {
		message = err.Error()
	}

	return &errorResponse{http.StatusConflict, message}
}

// Forbidden returns a forbidden response (403) with the given error.
func Forbidden(err error) Response {
	message := "not authorized"
	if err != nil {
		message = err.Error()
	}

	return &errorResponse{http.StatusForbidden, message}
}

// InternalError returns an internal error response (500) with the given error.
func InternalError(err error) Response {
	return &errorResponse{http.StatusInternalServerError, err.Error()}
}

// NotFound returns a not found response (404) with the given error.
func NotFound(err error) Response {
	message := "not found"
	if err != nil {
		message = err.Error()
	}

	return &errorResponse{http.StatusNotFound, message}
}

// NotImplemented returns a not implemented response (501) with the given error.
func NotImplemented(err error) Response {
	message := "not implemented"
	if err != nil {
		message = err.Error()
	}

	return &errorResponse{http.StatusNotImplemented, message}
}

// PreconditionFailed returns a precondition failed response (412) with the
// given error.
func PreconditionFailed(err error) Response {
	return &errorResponse{http.StatusPreconditionFailed, err.Error()}
}

// Unavailable return an unavailable response (503) with the given error.
func Unavailable(err error) Response {
	message := "unavailable"
	if err != nil {
		message = err.Error()
	}

	return &errorResponse{http.StatusServiceUnavailable, message}
}

func (r *errorResponse) String() string {
	return r.msg
}

func (r *errorResponse) Render(w http.ResponseWriter) error {
	var output io.Writer

	buf := &bytes.Buffer{}
	output = buf
	var captured *bytes.Buffer
	if debug {
		captured = &bytes.Buffer{}
		output = io.MultiWriter(buf, captured)
	}

	resp := api.ResponseRaw{
		Type:  api.ErrorResponse,
		Error: r.msg,
		Code:  r.code, // Set the error code in the Code field of the response body.
	}

	err := json.NewEncoder(output).Encode(resp)

	if err != nil {
		return err
	}

	if debug {
		debugLogger := logging.AddContext(logger.Log, log.Ctx{"http_code": r.code})
		util.DebugJSON("Error Response", captured, debugLogger)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	w.WriteHeader(r.code) // Set the error code in the HTTP header response.

	fmt.Fprintln(w, buf.String())

	return nil
}

// FileResponseEntry represents a file response entry.
type FileResponseEntry struct {
	Identifier string
	Path       string
	Filename   string
	Buffer     []byte /* either a path or a buffer must be provided */
}

type fileResponse struct {
	req              *http.Request
	files            []FileResponseEntry
	headers          map[string]string
	removeAfterServe bool
}

// FileResponse returns a new file response.
func FileResponse(r *http.Request, files []FileResponseEntry, headers map[string]string, removeAfterServe bool) Response {
	return &fileResponse{r, files, headers, removeAfterServe}
}

func (r *fileResponse) Render(w http.ResponseWriter) error {
	if r.headers != nil {
		for k, v := range r.headers {
			w.Header().Set(k, v)
		}
	}

	// No file, well, it's easy then
	if len(r.files) == 0 {
		return nil
	}

	// For a single file, return it inline
	if len(r.files) == 1 {
		var rs io.ReadSeeker
		var mt time.Time
		var sz int64

		if r.files[0].Path == "" {
			rs = bytes.NewReader(r.files[0].Buffer)
			mt = time.Now()
			sz = int64(len(r.files[0].Buffer))
		} else {
			f, err := os.Open(r.files[0].Path)
			if err != nil {
				return err
			}
			defer f.Close()

			fi, err := f.Stat()
			if err != nil {
				return err
			}

			mt = fi.ModTime()
			sz = fi.Size()
			rs = f
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", sz))
		w.Header().Set("Content-Disposition", fmt.Sprintf("inline;filename=%s", r.files[0].Filename))

		http.ServeContent(w, r.req, r.files[0].Filename, mt, rs)
		if r.files[0].Path != "" && r.removeAfterServe {
			err := os.Remove(r.files[0].Path)
			if err != nil {
				return err
			}
		}

		return nil
	}

	// Now the complex multipart answer.
	mw := multipart.NewWriter(w)
	defer mw.Close()

	w.Header().Set("Content-Type", mw.FormDataContentType())
	w.Header().Set("Transfer-Encoding", "chunked")

	for _, entry := range r.files {
		var rd io.Reader
		if entry.Path != "" {
			fd, err := os.Open(entry.Path)
			if err != nil {
				return err
			}
			defer fd.Close()

			rd = fd
		} else {
			rd = bytes.NewReader(entry.Buffer)
		}

		fw, err := mw.CreateFormFile(entry.Identifier, entry.Filename)
		if err != nil {
			return err
		}

		_, err = io.Copy(fw, rd)
		if err != nil {
			return err
		}
	}

	return nil
}

func (r *fileResponse) String() string {
	return fmt.Sprintf("%d files", len(r.files))
}

type forwardedResponse struct {
	client  lxd.InstanceServer
	request *http.Request
}

// ForwardedResponse takes a request directed to a node and forwards it to
// another node, writing back the response it gegs.
func ForwardedResponse(client lxd.InstanceServer, request *http.Request) Response {
	return &forwardedResponse{
		client:  client,
		request: request,
	}
}

func (r *forwardedResponse) Render(w http.ResponseWriter) error {
	info, err := r.client.GetConnectionInfo()
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s%s", info.Addresses[0], r.request.URL.RequestURI())
	forwarded, err := http.NewRequest(r.request.Method, url, r.request.Body)
	if err != nil {
		return err
	}

	for key := range r.request.Header {
		forwarded.Header.Set(key, r.request.Header.Get(key))
	}

	httpClient, err := r.client.GetHTTPClient()
	if err != nil {
		return err
	}

	response, err := httpClient.Do(forwarded)
	if err != nil {
		return err
	}

	for key := range response.Header {
		w.Header().Set(key, response.Header.Get(key))
	}

	w.WriteHeader(response.StatusCode)
	_, err = io.Copy(w, response.Body)
	return err
}

func (r *forwardedResponse) String() string {
	return fmt.Sprintf("request to %s", r.request.URL)
}

type manualResponse struct {
	hook func(w http.ResponseWriter) error
}

// ManualResponse creates a new manual response responder.
func ManualResponse(hook func(w http.ResponseWriter) error) Response {
	return &manualResponse{hook: hook}
}

func (r *manualResponse) Render(w http.ResponseWriter) error {
	return r.hook(w)
}

func (r *manualResponse) String() string {
	return "unknown"
}
