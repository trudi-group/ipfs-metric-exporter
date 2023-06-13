package metricplugin

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	pbmsg "github.com/ipfs/boxo/bitswap/message/pb"
	"github.com/ipfs/go-cid"
	"github.com/julienschmidt/httprouter"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/pkg/errors"
)

// HTTPServerConfig is the config for the metric export HTTP server.
type HTTPServerConfig struct {
	// ListenAddresses specifies the addresses to listen on.
	// It should take a form usable by `net.ResolveTCPAddr`, for example
	// `localhost:8181` or `0.0.0.0:1444`.
	ListenAddresses []string `json:"ListenAddresses"`
}

// API path constants.
const (
	APIBasePath                = "/metric_plugin/v1"
	APIPingPath                = APIBasePath + "/ping"
	APIBroadcastWantPath       = APIBasePath + "/broadcast_want"
	APIBroadcastCancelPath     = APIBasePath + "/broadcast_cancel"
	APIBroadcastWantCancelPath = APIBasePath + "/broadcast_want_cancel"
	APISamplePeerMetadataPath  = APIBasePath + "/sample_peer_metadata"
)

// API parameter constants.
const (
	// APIOnlyConnectedPeersParameter is the parameter for the SamplePeerMetadata function.
	APIOnlyConnectedPeersParameter = "only_connected"
)

type httpServer struct {
	api RPCAPI

	// A mutex to coordinate shutting down.
	closingLock sync.Mutex
	// A channel shared with all clients to notify them if we're shutting down.
	closing chan struct{}
	// A wait group to keep track of the clients.
	wg sync.WaitGroup

	// The underlying listeners.
	listeners []net.Listener
}

func newHTTPServer(api RPCAPI, cfg HTTPServerConfig) (*httpServer, error) {
	if len(cfg.ListenAddresses) == 0 {
		return nil, errors.New("missing ListenAddresses")
	}

	var listeners []net.Listener
	for _, addr := range cfg.ListenAddresses {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, errors.Wrap(err, "unable to bind")
		}
		log.Infof("metric export HTTP API running on http://%s%s", listener.Addr(), APIBasePath)

		listeners = append(listeners, listener)
	}

	return &httpServer{
		api:       api,
		listeners: listeners,
		closing:   make(chan struct{}),
		wg:        sync.WaitGroup{},
	}, nil
}

// Shutdown shuts down the HTTP server.
// This will close the listeners and wait for currently in-flight requests to
// finish.
func (s *httpServer) Shutdown() {
	s.closingLock.Lock()
	select {
	case <-s.closing:
		// Already closing/closed, no-op.
		s.closingLock.Unlock()
		return
	default:
		close(s.closing)
	}
	s.closingLock.Unlock()

	log.Info("shutting down HTTP server")

	// Shut down listeners.
	for _, l := range s.listeners {
		err := l.Close()
		if err != nil {
			log.Errorf("error closing listener: %s", err)
		}
	}

	// Wait for clients/in-progress calls to shut down.
	s.wg.Wait()
	log.Info("HTTP server shut down")
}

// StartServer starts the HTTP server.
func (s *httpServer) StartServer() {
	router := s.buildRoutes()

	for _, l := range s.listeners {
		s.wg.Add(1)
		go func(listener net.Listener) {
			defer s.wg.Done()
			err := http.Serve(listener, router)
			if err != nil {
				log.Errorf("unable to serve: %s", err)
			}
		}(l)
	}
}

func (s *httpServer) buildRoutes() *httprouter.Router {
	router := httprouter.New()

	router.GET("/", func(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
		_, _ = fmt.Fprintf(w, "check out %s\n", APIBasePath)
	})
	router.GET(APIBasePath, func(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
		_, _ = fmt.Fprintf(w, "Methods:\n"+
			"GET  %s no-op\n"+
			"POST %s broadcasts a Bitswap WANT message for a given list of CIDs\n"+
			"POST %s broadcasts a Bitswap CANCEL message for a given list of CIDs\n"+
			"POST %s broadcasts a Bitswap WANT message for a given list of CIDs, followed by a CANCEL message after a given amount of time\n"+
			"GET  %s returns information about peers from the peerstore, and connection state. The query parameter %s (bool) specifies whether to skip unconnected peers\n",
			APIPingPath,
			APIBroadcastWantPath,
			APIBroadcastCancelPath,
			APIBroadcastWantCancelPath,
			APISamplePeerMetadataPath,
			APIOnlyConnectedPeersParameter)
	})

	router.GET(APIPingPath,
		s.buildHandler(ping(s.api)))

	router.POST(APIBroadcastWantPath,
		s.buildHandler(broadcastBitswapWant(s.api)))

	router.POST(APIBroadcastCancelPath,
		s.buildHandler(broadcastBitswapCancel(s.api)))

	router.POST(APIBroadcastWantCancelPath,
		s.buildHandler(broadcastBitswapWantCancel(s.api)))

	router.GET(APISamplePeerMetadataPath,
		s.buildHandler(samplePeerMetadata(s.api)))

	return router
}

func (s *httpServer) buildHandler(f apiFunction) httprouter.Handle {
	return jsonEncodeResponse(
		extractStatusCode(
			ensurePresentableError(
				s.ensureNotClosedAndWaitGroup(
					recoverPanic(f)))))
}

// A JSONResponse is the format for every response returned by the HTTP server.
type JSONResponse struct {
	Status int         `json:"status"`
	Result interface{} `json:"result,omitempty"`
	Err    *string     `json:"error,omitempty"`
}

// presentableError is a user-presentable error.
// The error message will be returned to the user and the code will be used as
// the HTTP status code.
type presentableError interface {
	error
	code() int
}

type niceError struct {
	status int
	msg    string
}

func (e niceError) Error() string {
	return e.msg
}

func (e niceError) code() int {
	return e.status
}

func newPresentableError(code int, msg string) presentableError {
	return niceError{status: code, msg: msg}
}

// ErrServerClosing is returned when a request is made while the node has
// started to shut down.
var ErrServerClosing = newPresentableError(http.StatusServiceUnavailable, "server closing")

// ErrInternalServerError is returned when there was either a panic processing
// a request or a non-presentable error was generated during execution.
var ErrInternalServerError = newPresentableError(http.StatusInternalServerError, "internal server error")

// ErrInvalidRequest is returned for invalid requests, such as malformed JSON.
var ErrInvalidRequest = newPresentableError(http.StatusBadRequest, "invalid request")

// response is a marker interface for valid responses.
// In future versions this could maybe be replaced by generics.
type response interface {
	response()
}

type apiFunction func(r *http.Request, params httprouter.Params) (response, error)

type statusedResponseFunction func(r *http.Request, params httprouter.Params) (int, response, error)

// A decorator that ensures the server is not closing and, if not, wraps the
// given function in a wait group.
func (s *httpServer) ensureNotClosedAndWaitGroup(f apiFunction) apiFunction {
	return func(r *http.Request, p httprouter.Params) (response, error) {
		select {
		case <-s.closing:
			return nil, ErrServerClosing
		default:
		}

		s.wg.Add(1)
		defer s.wg.Done()

		return f(r, p)
	}
}

// A decorator that recovers from a panic and returns ErrInternalServerError.
func recoverPanic(f apiFunction) apiFunction {
	return func(r *http.Request, p httprouter.Params) (resp response, fErr error) {
		defer func() {
			err := recover()
			if err != nil {
				log.Errorf("recovered panic: %s", err)
				resp = nil
				fErr = ErrInternalServerError
			}
		}()

		return f(r, p)
	}
}

// A decorator that ensures errors are client-presentable.
// Non-presentable errors are replaced with ErrInternalServerError.
// After this, one of these cases applies:
// - either response is nil and error is a presentableError, or
// - response is not nil and error is nil.
func ensurePresentableError(f apiFunction) apiFunction {
	return func(r *http.Request, p httprouter.Params) (response, error) {
		resp, err := f(r, p)
		if err != nil {
			if resp != nil {
				panic("inner function returned an error and a non-nil response")
			}

			_, ok := err.(presentableError)
			if !ok {
				// Not a nice error. Replace with ISE.
				log.Errorf("recovered non-presentable error: %s", err)
				err = ErrInternalServerError
			}
		}

		return resp, err
	}
}

// A decorator that expands a (response, presentableError) tuple into a
// (HTTP status code, response, error) triple.
// If the error is not presentable, this panics.
func extractStatusCode(f apiFunction) statusedResponseFunction {
	return func(r *http.Request, p httprouter.Params) (int, response, error) {
		status := http.StatusOK
		resp, err := f(r, p)
		if err != nil {
			pErr, ok := err.(presentableError)
			if !ok {
				panic("API produced non-presentable error")
			}
			status = pErr.code()
		}

		return status, resp, err
	}
}

// A decorator that encodes a generated response into a JSONResponse.
func jsonEncodeResponse(f statusedResponseFunction) httprouter.Handle {
	return func(w http.ResponseWriter, h *http.Request, p httprouter.Params) {
		status, resp, err := f(h, p)

		w.WriteHeader(status)

		var errString *string
		if err != nil {
			s := err.Error()
			errString = &s
		}

		err = json.NewEncoder(w).Encode(JSONResponse{
			Status: status,
			Result: resp,
			Err:    errString,
		})

		if err != nil {
			log.Errorf("unable to write JSON response to %s: %s", h.RemoteAddr, err)
		}
	}
}

type broadcastBitswapWantResponse struct {
	Peers []broadcastBitswapWantResponseEntry `json:"peers"`
}

func (broadcastBitswapWantResponse) response() {}

type broadcastBitswapSendResponseEntry struct {
	TimestampBeforeSend time.Time `json:"timestamp_before_send"`
	SendDurationMillis  int64     `json:"send_duration_millis"`
	Error               *string   `json:"error,omitempty"`
}

type broadcastBitswapWantResponseEntry struct {
	broadcastBitswapSendResponseEntry
	Peer            peer.ID                          `json:"peer"`
	RequestTypeSent *pbmsg.Message_Wantlist_WantType `json:"request_type_sent,omitempty"`
}

type broadcastBitswapCancelResponse struct {
	Peers []broadcastBitswapCancelResponseEntry `json:"peers"`
}

func (broadcastBitswapCancelResponse) response() {}

type broadcastBitswapCancelResponseEntry struct {
	broadcastBitswapSendResponseEntry
	Peer peer.ID `json:"peer"`
}

type broadcastBitswapWantCancelResponse struct {
	Peers []broadcastBitswapWantCancelResponseEntry `json:"peers"`
}

func (broadcastBitswapWantCancelResponse) response() {}

type broadcastBitswapWantCancelWantEntry struct {
	broadcastBitswapSendResponseEntry
	RequestTypeSent *pbmsg.Message_Wantlist_WantType `json:"request_type_sent,omitempty"`
}

type broadcastBitswapWantCancelResponseEntry struct {
	Peer         peer.ID                             `json:"peer"`
	WantStatus   broadcastBitswapWantCancelWantEntry `json:"want_status"`
	CancelStatus broadcastBitswapSendResponseEntry   `json:"cancel_status"`
}

type pingResponse struct{}

func (pingResponse) response() {}

type broadcastBitswapRequest struct {
	Cids []cid.Cid `json:"cids"`
}

type broadcastBitswapWantCancelRequest struct {
	Cids                []cid.Cid `json:"cids"`
	SecondsBeforeCancel uint32    `json:"seconds_before_cancel"`
}

type samplePeerMetadataResponse struct {
	Timestamp    time.Time      `json:"timestamp"`
	NumConns     int            `json:"num_connections"`
	PeerMetadata []PeerMetadata `json:"peer_metadata"`
}

func (samplePeerMetadataResponse) response() {}

func ping(api RPCAPI) apiFunction {
	return func(_ *http.Request, _ httprouter.Params) (response, error) {
		api.Ping()

		return pingResponse{}, nil
	}
}

func broadcastBitswapWant(api RPCAPI) apiFunction {
	return func(r *http.Request, _ httprouter.Params) (response, error) {
		var req broadcastBitswapRequest

		err := json.NewDecoder(r.Body).Decode(&req)
		if err != nil {
			return nil, ErrInvalidRequest
		}

		res, err := api.BroadcastBitswapWant(req.Cids)
		if err != nil {
			if err == ErrBitswapProbeDisabled {
				return nil, newPresentableError(400, err.Error())
			}
			return nil, err
		}

		resp := make([]broadcastBitswapWantResponseEntry, 0, len(res))
		for _, entry := range res {
			var errMsg *string
			if entry.Error != nil {
				err := entry.Error.Error()
				errMsg = &err
			}
			resp = append(resp, broadcastBitswapWantResponseEntry{
				broadcastBitswapSendResponseEntry: broadcastBitswapSendResponseEntry{
					Error:               errMsg,
					TimestampBeforeSend: entry.TimestampBeforeSend,
					SendDurationMillis:  entry.SendDurationMillis,
				},
				Peer:            entry.Peer,
				RequestTypeSent: entry.RequestTypeSent,
			})
		}

		return broadcastBitswapWantResponse{Peers: resp}, nil
	}
}

func broadcastBitswapCancel(api RPCAPI) apiFunction {
	return func(r *http.Request, _ httprouter.Params) (response, error) {
		var req broadcastBitswapRequest

		err := json.NewDecoder(r.Body).Decode(&req)
		if err != nil {
			return nil, ErrInvalidRequest
		}

		res, err := api.BroadcastBitswapCancel(req.Cids)
		if err != nil {
			if err == ErrBitswapProbeDisabled {
				return nil, newPresentableError(400, err.Error())
			}
			return nil, err
		}

		resp := make([]broadcastBitswapCancelResponseEntry, 0, len(res))
		for _, entry := range res {
			var errMsg *string
			if entry.Error != nil {
				err := entry.Error.Error()
				errMsg = &err
			}
			resp = append(resp, broadcastBitswapCancelResponseEntry{
				broadcastBitswapSendResponseEntry: broadcastBitswapSendResponseEntry{
					Error:               errMsg,
					TimestampBeforeSend: entry.TimestampBeforeSend,
					SendDurationMillis:  entry.SendDurationMillis,
				},
				Peer: entry.Peer,
			})
		}

		return broadcastBitswapCancelResponse{Peers: resp}, nil
	}
}

func broadcastBitswapWantCancel(api RPCAPI) apiFunction {
	return func(r *http.Request, _ httprouter.Params) (response, error) {
		var req broadcastBitswapWantCancelRequest

		err := json.NewDecoder(r.Body).Decode(&req)
		if err != nil {
			return nil, ErrInvalidRequest
		}

		res, err := api.BroadcastBitswapWantCancel(req.Cids, uint(req.SecondsBeforeCancel))
		if err != nil {
			if err == ErrBitswapProbeDisabled {
				return nil, newPresentableError(400, err.Error())
			}
			return nil, err
		}

		resp := make([]broadcastBitswapWantCancelResponseEntry, 0, len(res))
		for _, entry := range res {
			var wantErrMsg, cancelErrMsg *string
			if entry.CancelStatus.Error != nil {
				err := entry.CancelStatus.Error.Error()
				cancelErrMsg = &err
			}
			if entry.WantStatus.Error != nil {
				err := entry.WantStatus.Error.Error()
				wantErrMsg = &err
			}
			resp = append(resp, broadcastBitswapWantCancelResponseEntry{
				CancelStatus: broadcastBitswapSendResponseEntry{
					Error:               cancelErrMsg,
					TimestampBeforeSend: entry.CancelStatus.TimestampBeforeSend,
					SendDurationMillis:  entry.CancelStatus.SendDurationMillis,
				},
				WantStatus: broadcastBitswapWantCancelWantEntry{
					broadcastBitswapSendResponseEntry: broadcastBitswapSendResponseEntry{
						Error:               wantErrMsg,
						TimestampBeforeSend: entry.WantStatus.TimestampBeforeSend,
						SendDurationMillis:  entry.WantStatus.SendDurationMillis,
					},
					RequestTypeSent: entry.WantStatus.RequestTypeSent,
				},
				Peer: entry.Peer,
			})
		}

		return broadcastBitswapWantCancelResponse{Peers: resp}, nil
	}
}

func samplePeerMetadata(api RPCAPI) apiFunction {
	return func(req *http.Request, _ httprouter.Params) (response, error) {
		onlyConnected := strings.ToLower(req.URL.Query().Get(APIOnlyConnectedPeersParameter)) == "true"
		pmd, conns := api.SamplePeerMetadata(onlyConnected)
		ts := time.Now().UTC()

		return samplePeerMetadataResponse{
			Timestamp:    ts,
			NumConns:     conns,
			PeerMetadata: pmd,
		}, nil
	}
}
