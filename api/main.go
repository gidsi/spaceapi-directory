package main

import (
	"encoding/json"
	"flag"
	"github.com/felixge/httpsnoop"
	"github.com/itchyny/gojq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/cors"
	"goji.io"
	"goji.io/pat"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"strconv"
)

//go:generate go run scripts/generateOpenApi.go

type entry struct {
	Url              string            `json:"url"`
	Valid            bool              `json:"valid"`
	Space            string            `json:"space,omitempty"`
	LastSeen         int64             `json:"lastSeen,omitempty"`
	ErrMsg           []string          `json:"errMsg,omitempty"`
	Data             interface{}       `json:"data,omitempty"`
	ValidationResult *validationResult `json:"validationResult,omitempty"`
}

type validationResult struct {
	Valid       bool `json:"valid"`
	IsHttps     bool `json:"isHttps"`
	HttpForward bool `json:"httpsForward"`
	Reachable   bool `json:"reachable"`
	Cors        bool `json:"cors"`
	ContentType bool `json:"contentType"`
	CertValid   bool `json:"certValid"`
}

type collectorEntry struct {
	Url              string            `json:"url"`
	Valid            bool              `json:"valid"`
	LastSeen         int64             `json:"lastSeen,omitempty"`
	ErrMsg           []string          `json:"errMsg,omitempty"`
	Data             interface{}       `json:"data,omitempty"`
	ValidationResult *validationResult `json:"validationResult,omitempty"`
}

var (
	httpRequestSummary = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name: "spaceapi_http_requests",
			Help: "All the http requests!",
		},
		[]string{"method", "route", "code"},
	)
	spaceApiCollectorUrl string
)

func init() {
	prometheus.MustRegister(httpRequestSummary)

	flag.StringVar(
		&spaceApiCollectorUrl,
		"collectorUrl",
		"http://collector:8080",
		"Url to the collector service",
	)

	flag.Parse()
}

func main() {
	c := cors.New(cors.Options{
		AllowedOrigins: []string{"*"},
	})

	mux := goji.NewMux()
	mux.Use(c.Handler)
	mux.Use(statisticMiddelware)

	mux.Handle(pat.Get("/metrics"), promhttp.Handler())

	mux.HandleFunc(pat.Get("/"), serveV1)
	mux.HandleFunc(pat.Get("/v1"), serveV1)
	mux.HandleFunc(pat.Get("/v2"), serveV2)
	mux.HandleFunc(pat.Get("/cache"), serveCache)
	mux.HandleFunc(pat.Get("/openapi.json"), openApi)

	log.Println("starting api...")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

func openApi(writer http.ResponseWriter, _ *http.Request) {
	_, err := writer.Write([]byte(openapi))
	if err != nil {
		writer.WriteHeader(500)
	}
}

func getFilter(r *http.Request) (bool, bool) {
	validFilterQuery := r.URL.Query().Get("valid")

	if validFilterQuery == "all" {
		return false, true
	} else if validFilterQuery != "" {
		validFilter, err := strconv.ParseBool(validFilterQuery)
		if err != nil {
			return true, false
		}

		return validFilter, false
	}

	return true, false
}

func getJQFilter(r *http.Request) string {
	filter := r.URL.Query().Get("filter")
	if filter != "" {
		return `.[] | select(` + filter + `)`
	}

	return ".[]"
}

func serveV1(w http.ResponseWriter, r *http.Request) {
	validFilter, noFilter := getFilter(r)
	if err := json.NewEncoder(w).Encode(func() interface{} {
		response := make(map[string]string)
		for _, entry := range getDirectory(getJQFilter(r)) {
			if entry.Valid == validFilter || noFilter == true {
				if entry.Data != nil {
					spaceName := entry.Data.(map[string]interface{})["space"]
					response[spaceName.(string)] = entry.Url
				} else {
					response["unknown_"+strconv.FormatInt(rand.Int63(), 10)] = entry.Url
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		return response
	}()); err != nil {
		panic(err)
	}
}

func serveV2(w http.ResponseWriter, r *http.Request) {
	validFilter, noFilter := getFilter(r)
	includeDataParam := r.URL.Query().Get("includeData")
	includeData, err := strconv.ParseBool(includeDataParam)
	if err != nil {
		includeData = false
	}

	includeValidationResultParam := r.URL.Query().Get("includeValidationResult")
	includeValidationResult, err := strconv.ParseBool(includeValidationResultParam)
	if err != nil {
		includeValidationResult = false
	}

	if err := json.NewEncoder(w).Encode(func() []entry {
		var response []entry
		for _, collectorEntry := range getDirectory(getJQFilter(r)) {
			if collectorEntry.Valid == validFilter || noFilter == true {
				var spaceName string
				if collectorEntry.Data != nil {
					spaceName = collectorEntry.Data.(map[string]interface{})["space"].(string)
				} else {
					spaceName = ""
				}

				var data interface{}
				if includeData {
					data = collectorEntry.Data
				}

				var validationResult *validationResult
				if includeValidationResult {
					validationResult = collectorEntry.ValidationResult
				}

				response = append(response, entry{
					collectorEntry.Url,
					collectorEntry.Valid,
					spaceName,
					collectorEntry.LastSeen,
					collectorEntry.ErrMsg,
					data,
					validationResult,
				})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		return response
	}()); err != nil {
		panic(err)
	}
}

func serveCache(w http.ResponseWriter, r *http.Request) {
	validFilter, noFilter := getFilter(r)
	if err := json.NewEncoder(w).Encode(func() []collectorEntry {
		var response []collectorEntry
		for _, collectorEntry := range getDirectory(getJQFilter(r)) {
			if collectorEntry.Valid == validFilter || noFilter == true {
				response = append(response, collectorEntry)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		return response
	}()); err != nil {
		panic(err)
	}
}

func statisticMiddelware(inner http.Handler) http.Handler {
	mw := func(w http.ResponseWriter, r *http.Request) {
		m := httpsnoop.CaptureMetrics(inner, w, r)
		httpRequestSummary.With(prometheus.Labels{"method": r.Method, "route": r.URL.Path, "code": strconv.Itoa(m.Code)}).Observe(m.Duration.Seconds())
	}
	return http.HandlerFunc(mw)
}

func getDirectory(filter string) []collectorEntry {
	var staticDirectory []collectorEntry
	resp, err := http.Get(spaceApiCollectorUrl)
	if err != nil {
		log.Println(err)
	}
	defer func() {
		err := resp.Body.Close()
		if err != nil {
			panic(err)
		}
	}()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Println(err)
	}

	query, err := gojq.Parse(filter)
	if err != nil {
		log.Println(err)
	}

	var m interface{}
	err = json.Unmarshal(body, &m)

	iter := query.Run(m)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, ok := v.(error); ok {
			log.Println(err)
		}

		marshal, err := json.Marshal(v)
		if err != nil {
			continue
		}

		var entry collectorEntry
		err = json.Unmarshal(marshal, &entry)
		if err != nil {
			continue
		}

		staticDirectory = append(staticDirectory, entry)
	}

	return staticDirectory
}
