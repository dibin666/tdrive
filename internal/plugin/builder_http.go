package plugin

import (
	"encoding/json"
	"net/http"
)

// BuilderHandler exposes only the two operations needed by the host. The
// standalone command binds this handler to a Unix socket or loopback address;
// it is never intended to be a public HTTP service.
func BuilderHandler(builder *SourceBuilder) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			writeBuilderError(response, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		response.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/inspect", func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			writeBuilderError(response, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var builderRequest BuilderRequest
		if err := decodeBuilderRequest(response, request, &builderRequest); err != nil {
			return
		}
		result, err := builder.Inspect(request.Context(), builderRequest)
		if err != nil {
			writeBuilderError(response, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeBuilderJSON(response, http.StatusOK, result)
	})
	mux.HandleFunc("/v1/build", func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			writeBuilderError(response, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var builderRequest BuilderRequest
		if err := decodeBuilderRequest(response, request, &builderRequest); err != nil {
			return
		}
		result, err := builder.Build(request.Context(), builderRequest)
		if err != nil {
			writeBuilderError(response, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeBuilderJSON(response, http.StatusOK, result)
	})
	return mux
}

func decodeBuilderRequest(response http.ResponseWriter, request *http.Request, target any) error {
	request.Body = http.MaxBytesReader(response, request.Body, 1<<20)
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(target); err != nil {
		writeBuilderError(response, http.StatusBadRequest, err.Error())
		return err
	}
	return nil
}

func writeBuilderJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func writeBuilderError(response http.ResponseWriter, status int, message string) {
	writeBuilderJSON(response, status, map[string]string{"error": message})
}
