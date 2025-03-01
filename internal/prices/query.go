package prices

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/infracost/infracost/internal/config"
	"github.com/infracost/infracost/internal/schema"
	"github.com/pkg/errors"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

var ErrInvalidAPIKey = errors.New("Invalid API key")

type PricingAPIError struct {
	err error
	msg string
}

func (e *PricingAPIError) Error() string {
	return fmt.Sprintf("%s: %v", e.msg, e.err.Error())
}

type pricingAPIErrorResponse struct {
	Error string `json:"error"`
}

type queryKey struct {
	Resource      *schema.Resource
	CostComponent *schema.CostComponent
}

type QueryResult struct {
	queryKey
	Result gjson.Result
}

type GraphQLQuery struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables"`
}

type QueryRunner interface {
	RunQueries(resource *schema.Resource) ([]QueryResult, error)
}

type GraphQLQueryRunner struct {
	endpoint string
	apiKey   string
}

func NewGraphQLQueryRunner(endpoint string, apiKey string) *GraphQLQueryRunner {
	return &GraphQLQueryRunner{
		endpoint: endpoint,
		apiKey:   apiKey,
	}
}

func (q *GraphQLQueryRunner) RunQueries(r *schema.Resource) ([]QueryResult, error) {
	keys, queries := q.batchQueries(r)

	if len(queries) == 0 {
		log.Debugf("Skipping getting pricing details for %s since there are no queries to run", r.Name)
		return []QueryResult{}, nil
	}

	log.Debugf("Getting pricing details from %s for %s", q.endpoint, r.Name)

	results, err := q.getQueryResults(queries)
	if err != nil {
		return []QueryResult{}, err
	}

	return q.zipQueryResults(keys, results), nil
}

func (q *GraphQLQueryRunner) buildQuery(product *schema.ProductFilter, price *schema.PriceFilter) GraphQLQuery {
	v := map[string]interface{}{}
	v["productFilter"] = product
	v["priceFilter"] = price

	query := `
		query($productFilter: ProductFilter!, $priceFilter: PriceFilter) {
			products(filter: $productFilter) {
				prices(filter: $priceFilter) {
					priceHash
					USD
				}
			}
		}
	`

	return GraphQLQuery{query, v}
}

func (q *GraphQLQueryRunner) getQueryResults(queries []GraphQLQuery) ([]gjson.Result, error) {
	results := make([]gjson.Result, 0, len(queries))

	if len(queries) == 0 {
		log.Debug("skipping GraphQL request as no queries have been specified")
		return results, nil
	}

	queriesBody, err := json.Marshal(queries)
	if err != nil {
		return results, errors.Wrap(err, "Error generating request for pricing API")
	}

	req, err := http.NewRequest("POST", q.endpoint, bytes.NewBuffer(queriesBody))
	if err != nil {
		return results, errors.Wrap(err, "Error generating request for pricing API")
	}

	config.AddAuthHeaders(q.apiKey, req)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return results, errors.Wrap(err, "Error sending request to pricing API")
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return results, &PricingAPIError{err, "Invalid response from pricing API"}
	}
	if resp.StatusCode != 200 {
		var r pricingAPIErrorResponse
		err = json.Unmarshal(body, &r)
		if err != nil {
			return results, &PricingAPIError{err, "Invalid response from pricing API"}
		}
		if r.Error == "Invalid API key" {
			return results, ErrInvalidAPIKey
		}
		return results, &PricingAPIError{errors.New(r.Error), "Received error from pricing API"}
	}

	results = append(results, gjson.ParseBytes(body).Array()...)

	return results, nil
}

// Batch all the queries for this resource so we can use one GraphQL call.
// Use queryKeys to keep track of which query maps to which sub-resource and price component.
func (q *GraphQLQueryRunner) batchQueries(r *schema.Resource) ([]queryKey, []GraphQLQuery) {
	keys := make([]queryKey, 0)
	queries := make([]GraphQLQuery, 0)

	for _, c := range r.CostComponents {
		keys = append(keys, queryKey{r, c})
		queries = append(queries, q.buildQuery(c.ProductFilter, c.PriceFilter))
	}

	for _, r := range r.FlattenedSubResources() {
		for _, c := range r.CostComponents {
			keys = append(keys, queryKey{r, c})
			queries = append(queries, q.buildQuery(c.ProductFilter, c.PriceFilter))
		}
	}

	return keys, queries
}

func (q *GraphQLQueryRunner) zipQueryResults(k []queryKey, r []gjson.Result) []QueryResult {
	res := make([]QueryResult, 0, len(k))

	for i, k := range k {
		res = append(res, QueryResult{
			queryKey: k,
			Result:   r[i],
		})
	}

	return res
}
