package mqtt

// MethodFunc is the signature method-specific RPC handlers
// implement. Return (nil, nil) to fall through to the fallback
// handler; return (bytes, nil) to send those bytes back to UDM.
// On error, the registry logs and falls back to the default.
type MethodFunc func(requestID string, body []byte) ([]byte, error)

// MethodRegistry dispatches RPC requests to method-specific
// handlers, falling back to the configured default for any path
// without a registered handler. Implements the RPCHandler
// interface so it can be passed directly to Client.SetHandler.
type MethodRegistry struct {
	handlers map[string]MethodFunc
	fallback RPCHandler
	log      Logger
}

// NewMethodRegistry constructs a registry with the given fallback
// (typically a DefaultHandler) and logger.
func NewMethodRegistry(fallback RPCHandler, log Logger) *MethodRegistry {
	if fallback == nil {
		fallback = DefaultHandler{}
	}
	return &MethodRegistry{
		handlers: make(map[string]MethodFunc),
		fallback: fallback,
		log:      log,
	}
}

// Register installs a method-specific handler for path. Overwrites
// any prior registration silently; callers should register each
// path exactly once at startup.
func (r *MethodRegistry) Register(path string, h MethodFunc) {
	r.handlers[path] = h
}

// Handle satisfies RPCHandler. Looks up path in the registry; on
// miss or error, returns the fallback's response so UDM always
// receives a well-formed reply.
func (r *MethodRegistry) Handle(path, requestID string, body []byte) []byte {
	h, ok := r.handlers[path]
	if !ok {
		return r.fallback.Handle(path, requestID, body)
	}
	resp, err := h(requestID, body)
	if err != nil {
		if r.log != nil {
			r.log.Warnf("mqtt: handler %s body decode failed: %v (falling back to default)", path, err)
		}
		return r.fallback.Handle(path, requestID, body)
	}
	if resp == nil {
		return r.fallback.Handle(path, requestID, body)
	}
	return resp
}
