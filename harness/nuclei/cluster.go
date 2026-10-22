package nuclei

// cluster.go runs a Nuclei cluster attack.
//
// A cluster attack fires the same request once per payload, plus once with
// no payload at all, and looks for the payload whose answer differs from
// the baseline. It is how a template finds a payload that got past a
// filter: the baseline says "blocked" and the payload says something else,
// and the difference is the finding.
//
// The comparison is on one attribute - the word the body carries, the
// status code, or the body size - and the condition decides how many
// payloads have to differ for the cluster to fire.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"ceraa/harness/recon"
)

// Cluster is a cluster attack.
type Cluster struct {
	// Method is the method of the request.
	Method string
	// Payloads maps a payload name to the value that is sent.
	Payloads map[string]string
	// Attribute is what the responses are compared on: "word", "status" or
	// "size".
	Attribute string
	// Words are the markers the "word" attribute looks for.
	Words []string
	// Condition decides how many payloads have to differ: "and" (all of
	// them) or "or" (any of them).
	Condition string
}

// ClusterResponse is the part of a response the cluster compares.
type ClusterResponse struct {
	Status int
	Body   []byte
	URL    string
	// SHA256 and ReceiptID are the ones of the observation, so a cluster
	// finding can cite the request that produced it like any other.
	SHA256    string
	ReceiptID string
}

// ClusterPayloadResponse is one payload's response.
type ClusterPayloadResponse struct {
	// Name is the name of the payload.
	Name string
	// Value is the value that was sent.
	Value string
	// Response is what came back.
	Response ClusterResponse
}

// ClusterOutlier is one payload whose response differed from the baseline.
type ClusterOutlier struct {
	// Payload is the name of the payload.
	Payload string
	// Value is the value that was sent.
	Value string
	// Attribute is what was compared.
	Attribute string
	// Baseline is the baseline's value of the attribute.
	Baseline string
	// Observed is the payload's value of the attribute.
	Observed string
	// Difference is the difference in words.
	Difference string
	// URL is where the payload was sent.
	URL string
	// Status is the status the payload's request answered with.
	Status int
	// SHA256 and ReceiptID cite the observation.
	SHA256    string
	ReceiptID string
}

// ClusterResult is the outcome of a cluster attack.
type ClusterResult struct {
	// Matched says whether the cluster fired.
	Matched bool
	// Attribute is what was compared.
	Attribute string
	// Baseline is the response of the request without a payload.
	Baseline ClusterResponse
	// Outliers are the payloads that differed from the baseline.
	Outliers []ClusterOutlier
	// Missing names the variables that rendered to nothing.
	Missing []string
}

// ClusterRequest is the request a cluster attack fires.
type ClusterRequest struct {
	Method string
	// Path is the raw path, with the template's variables still in it.
	Path    string
	Headers map[string]string
	Body    string
	Timeout int
	// Target is the target the path is joined onto. It is nil when the
	// path is already absolute.
	Target *url.URL
}

// ClusterFire is one request of a cluster attack: the baseline, or one
// payload's request.
type ClusterFire struct {
	// Payload is the name of the payload, empty for the baseline.
	Payload string
	// Value is the payload's value, empty for the baseline.
	Value string
	// Request is the request to send.
	Request recon.Request
	// Missing names the variables that rendered to nothing.
	Missing []string
}

// ClusterRequests builds the requests of a cluster attack: the baseline and
// one per payload.
//
// The payload value goes where the template asked for it. A request that
// names {{payload}} gets the value there; a request that does not gets it
// as a query parameter, because a cluster attack has to change something
// and the path is the one thing every request has.
//
// The variables are rendered here, after the payload went in: a template
// that writes {{payload}} in a path would otherwise have the placeholder
// resolved away as an unknown variable.
func ClusterRequests(cluster Cluster, request ClusterRequest, vars Vars) ([]ClusterFire, error) {
	method := strings.ToUpper(strings.TrimSpace(request.Method))
	if method == "" {
		method = http.MethodGet
	}
	base, missing, err := renderClusterRequest(request, method, vars)
	if err != nil {
		return nil, err
	}
	fires := []ClusterFire{{Request: base, Missing: missing}}
	names := make([]string, 0, len(cluster.Payloads))
	for name := range cluster.Payloads {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		value := cluster.Payloads[name]
		payload := applyClusterPayload(request, name, value)
		rendered, gaps, err := renderClusterRequest(payload, method, vars)
		if err != nil {
			return nil, err
		}
		fires = append(fires, ClusterFire{
			Payload: name,
			Value:   value,
			Request: rendered,
			Missing: gaps,
		})
	}
	return fires, nil
}

// applyClusterPayload puts the payload value into the request.
func applyClusterPayload(request ClusterRequest, name, value string) ClusterRequest {
	out := request
	out.Path = strings.ReplaceAll(request.Path, "{{payload}}", value)
	out.Body = strings.ReplaceAll(request.Body, "{{payload}}", value)
	headers := make(map[string]string, len(request.Headers))
	for header, headerValue := range request.Headers {
		headers[header] = strings.ReplaceAll(headerValue, "{{payload}}", value)
	}
	out.Headers = headers
	if out.Path == request.Path {
		out.Path = appendPayloadParam(request.Path, name, value)
	}
	return out
}

// renderClusterRequest renders the variables into one request of a cluster
// attack.
func renderClusterRequest(request ClusterRequest, method string, vars Vars) (recon.Request, []string, error) {
	renderedPath, gaps := Render(request.Path, vars)
	renderedBody, bodyGaps := Render(request.Body, vars)
	missing := append(gaps, bodyGaps...)
	renderedHeaders := make(map[string]string, len(request.Headers))
	for name, value := range request.Headers {
		rendered, headerGaps := Render(value, vars)
		renderedHeaders[name] = rendered
		missing = append(missing, headerGaps...)
	}
	target := renderedPath
	if request.Target != nil {
		target = joinURL(request.Target, renderedPath)
	}
	return recon.Request{
		URL:     target,
		Method:  method,
		Headers: renderedHeaders,
		Body:    renderedBody,
		Timeout: request.Timeout,
	}, missing, nil
}

// appendPayloadParam adds a query parameter to a path.
func appendPayloadParam(path, name, value string) string {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return path + separator + url.QueryEscape(name) + "=" + url.QueryEscape(value)
}

// RunCluster fires the requests of a cluster attack and compares the
// responses.
//
// fire is the request seam: the runner passes the gated, budgeted probe,
// so a cluster attack spends the budget and writes the receipts of every
// other request.
func RunCluster(ctx context.Context, cluster Cluster, request ClusterRequest, vars Vars, fire func(context.Context, recon.Request) (recon.Result, []byte, error)) (ClusterResult, error) {
	fires, err := ClusterRequests(cluster, request, vars)
	if err != nil {
		return ClusterResult{}, err
	}
	var (
		baseline     ClusterResponse
		baselineSeen bool
		payloads     []ClusterPayloadResponse
		missing      []string
	)
	for _, item := range fires {
		result, body, err := fire(ctx, item.Request)
		if err != nil {
			// The seam has already recorded the failure. The comparison
			// carries on with what did come back: one payload that was
			// refused does not decide the other ones.
			continue
		}
		response := ClusterResponse{
			Status:    result.Status,
			Body:      body,
			URL:       result.URL,
			SHA256:    result.SHA256,
			ReceiptID: result.ReceiptID,
		}
		if response.URL == "" {
			response.URL = item.Request.URL
		}
		if item.Payload == "" {
			baseline = response
			baselineSeen = true
			missing = item.Missing
			continue
		}
		payloads = append(payloads, ClusterPayloadResponse{
			Name:     item.Payload,
			Value:    item.Value,
			Response: response,
		})
	}
	if !baselineSeen {
		return ClusterResult{}, fmt.Errorf("the baseline request was not answered, so the responses cannot be compared")
	}
	comparison := CompareCluster(cluster, baseline, payloads)
	comparison.Missing = missing
	return comparison, nil
}

// CompareCluster compares the payload responses against the baseline.
//
// A payload is an outlier when the attribute it is compared on differs
// from the baseline's. The condition decides how many of them have to
// differ for the cluster to fire: "or", the default, needs one, and "and"
// needs every one.
func CompareCluster(cluster Cluster, baseline ClusterResponse, payloads []ClusterPayloadResponse) ClusterResult {
	attribute := strings.ToLower(strings.TrimSpace(cluster.Attribute))
	if attribute == "" {
		attribute = "word"
	}
	baselineValue := clusterAttributeValue(attribute, cluster.Words, baseline)
	var outliers []ClusterOutlier
	for _, payload := range payloads {
		observed := clusterAttributeValue(attribute, cluster.Words, payload.Response)
		if observed == baselineValue {
			continue
		}
		outliers = append(outliers, ClusterOutlier{
			Payload:   payload.Name,
			Value:     payload.Value,
			Attribute: attribute,
			Baseline:  baselineValue,
			Observed:  observed,
			Difference: fmt.Sprintf("the %s of the baseline is %s and the %s of the payload %q is %s",
				attribute, clusterAttributeText(baselineValue), attribute, payload.Name, clusterAttributeText(observed)),
			URL:       payload.Response.URL,
			Status:    payload.Response.Status,
			SHA256:    payload.Response.SHA256,
			ReceiptID: payload.Response.ReceiptID,
		})
	}
	matched := len(outliers) > 0
	if strings.EqualFold(strings.TrimSpace(cluster.Condition), "and") {
		matched = len(outliers) > 0 && len(outliers) == len(payloads)
	}
	return ClusterResult{
		Matched:   matched,
		Attribute: attribute,
		Baseline:  baseline,
		Outliers:  outliers,
	}
}

// clusterAttributeValue reads the value of the compared attribute.
func clusterAttributeValue(attribute string, words []string, response ClusterResponse) string {
	switch attribute {
	case "status":
		return strconv.Itoa(response.Status)
	case "size":
		return strconv.Itoa(len(response.Body))
	case "word":
		body := string(response.Body)
		for _, word := range words {
			if word == "" {
				continue
			}
			if strings.Contains(body, word) {
				return word
			}
		}
		return ""
	}
	return ""
}

// clusterAttributeText renders an attribute value for a sentence.
func clusterAttributeText(value string) string {
	if value == "" {
		return "(none)"
	}
	return strconv.Quote(value)
}
