package cloudsim

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
)

// The client and the simulator are two readings of one API by the same
// people, so contract_test.go catches a typo on either side but not a shared
// misunderstanding. The public OpenAPI document is a third reading, by
// somebody else, and every contract test runs through it: each request the
// real client sends and each answer the simulator gives must match it.
//
// It is not the authority. It has been wrong before, and RavenDB Cloud's
// source settles a disagreement. But a request it calls malformed is worth a
// look every time.

var swagger struct {
	once   sync.Once
	router routers.Router
	err    error
}

func swaggerRouter(t *testing.T) routers.Router {
	t.Helper()
	swagger.once.Do(func() {
		loader := openapi3.NewLoader()
		doc, err := loader.LoadFromFile("../../.reference/swagger.json")
		if err != nil {
			swagger.err = err
			return
		}
		if err := doc.Validate(loader.Context); err != nil {
			swagger.err = err
			return
		}
		swagger.router, swagger.err = legacy.NewRouter(doc)
	})
	if swagger.err != nil {
		t.Fatalf("loading the OpenAPI document: %v", swagger.err)
	}
	return swagger.router
}

// checkedAgainstSwagger serves next and reports, on t, every request or answer
// that does not match the OpenAPI document. The /_lab surface is the
// simulator's own and is passed through.
func checkedAgainstSwagger(t *testing.T, next http.Handler) http.Handler {
	router := swaggerRouter(t)
	options := &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_lab/") {
			next.ServeHTTP(w, r)
			return
		}
		route, params, err := router.FindRoute(r)
		if err != nil {
			t.Errorf("%s %s is not in the OpenAPI document: %v", r.Method, r.URL.Path, err)
			next.ServeHTTP(w, r)
			return
		}

		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		request := &openapi3filter.RequestValidationInput{
			Request: r, PathParams: params, Route: route, Options: options,
		}
		if err := openapi3filter.ValidateRequest(context.Background(), request); err != nil {
			t.Errorf("the client's %s %s does not match the OpenAPI document: %v", r.Method, r.URL.Path, err)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		answer := httptest.NewRecorder()
		next.ServeHTTP(answer, r)

		response := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: request,
			Status:                 answer.Code,
			Header:                 answer.Header(),
			Body:                   io.NopCloser(bytes.NewReader(answer.Body.Bytes())),
			Options:                options,
		}
		if err := openapi3filter.ValidateResponse(context.Background(), response); err != nil {
			t.Errorf("the simulator's answer to %s %s does not match the OpenAPI document: %v",
				r.Method, r.URL.Path, err)
		}

		for k, v := range answer.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(answer.Code)
		_, _ = w.Write(answer.Body.Bytes())
	})
}
