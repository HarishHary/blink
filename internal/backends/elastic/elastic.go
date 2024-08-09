package elastic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esapi"
	"github.com/harishhary/blink/internal/backends"
	"github.com/harishhary/blink/internal/helpers"
	"github.com/harishhary/blink/pkg/alerts"
)

type ElasticsearchBackend struct {
	ctx    context.Context
	client *elasticsearch.Client
	index  string
}

func NewElasticsearchBackend(ctx context.Context, index string) (*ElasticsearchBackend, error) {
	client, err := elasticsearch.NewDefaultClient()
	if err != nil {
		return nil, err
	}
	return &ElasticsearchBackend{
		ctx:    ctx,
		client: client,
		index:  index,
	}, nil
}

func (es *ElasticsearchBackend) AddAlerts(alerts []*alerts.Alert) error {
	var buf bytes.Buffer
	for _, alert := range alerts {
		record, err := es.ToRecord(alert)
		if err != nil {
			return fmt.Errorf("error marshaling alert to record: %w", err)
		}

		meta := []byte(fmt.Sprintf(`{ "index" : { "_index" : "%s", "_id" : "%s_%s" } }%s`, es.index, record["RuleName"], record["AlertID"], "\n"))
		data, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("error marshaling record: %w", err)
		}
		data = append(data, "\n"...)
		buf.Grow(len(meta) + len(data))
		buf.Write(meta)
		buf.Write(data)
	}
	req := esapi.BulkRequest{
		Body: bytes.NewReader(buf.Bytes()),
	}
	res, err := req.Do(es.ctx, es.client)
	if err != nil {
		return fmt.Errorf("bulk request failed: %w", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("bulk request error: %s", body)
	}

	return nil
}

func (es *ElasticsearchBackend) DeleteAlerts(alerts []*alerts.Alert) error {
	var buf bytes.Buffer
	for _, alert := range alerts {
		recordKey := alert.RecordKey()
		docID := fmt.Sprintf("%s_%s", recordKey["RuleName"], recordKey["AlertID"])
		deleteReq := map[string]interface{}{
			"delete": map[string]interface{}{
				"_index": es.index,
				"_id":    docID,
			},
		}
		deleteReqBytes, _ := json.Marshal(deleteReq)
		buf.Write(deleteReqBytes)
		buf.WriteString("\n")
	}
	req := esapi.BulkRequest{
		Body: bytes.NewReader(buf.Bytes()),
	}
	res, err := req.Do(es.ctx, es.client)
	if err != nil {
		return fmt.Errorf("bulk delete request failed: %w", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("bulk delete request error: %s", body)
	}

	return nil
}

func (es *ElasticsearchBackend) UpdateSentOutputs(alert *alerts.Alert) error {
	recordKey := alert.RecordKey()
	docID := fmt.Sprintf("%s_%s", recordKey["RuleName"], recordKey["AlertID"])

	update := map[string]interface{}{
		"doc": map[string]interface{}{
			"OutputsSent": alert.OutputsSent,
		},
	}
	updateBytes, err := json.Marshal(update)
	if err != nil {
		return fmt.Errorf("error marshaling update: %w", err)
	}

	req := esapi.UpdateRequest{
		Index:      es.index,
		DocumentID: docID,
		Body:       bytes.NewReader(updateBytes),
	}

	res, err := req.Do(es.ctx, es.client)
	if err != nil {
		return fmt.Errorf("update request failed: %w", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("update request error: %s", body)
	}

	return nil
}

func (es *ElasticsearchBackend) GetAlertRecords(ruleName string, alertProcTimeoutSec int) <-chan backends.Record {
	out := make(chan backends.Record)
	go func() {
		defer close(out)

		query := map[string]interface{}{
			"query": map[string]interface{}{
				"bool": map[string]interface{}{
					"must": []map[string]interface{}{
						{
							"term": map[string]interface{}{
								"RuleName.keyword": ruleName,
							},
						},
						{
							"range": map[string]interface{}{
								"Dispatched": map[string]interface{}{
									"gte": fmt.Sprintf("now-%ds", alertProcTimeoutSec),
								},
							},
						},
					},
				},
			},
			"sort": []map[string]interface{}{
				{
					"Dispatched": map[string]interface{}{
						"order": "asc",
					},
				},
			},
		}

		queryBytes, _ := json.Marshal(query)
		req := esapi.SearchRequest{
			Index:  []string{es.index},
			Body:   bytes.NewReader(queryBytes),
			Scroll: 2 * time.Minute, // set scroll timeout to 2 minutes
		}

		res, err := req.Do(es.ctx, es.client)
		if err != nil {
			fmt.Printf("Error searching records: %v\n", err)
			return
		}
		defer res.Body.Close()

		if res.IsError() {
			body, _ := io.ReadAll(res.Body)
			fmt.Printf("search request error: %s\n", body)
			return
		}

		var scrollID string
		for {
			var result struct {
				ScrollID string `json:"_scroll_id"`
				Hits     struct {
					Hits []struct {
						Source backends.Record `json:"_source"`
					} `json:"hits"`
				} `json:"hits"`
			}
			if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
				fmt.Printf("Error decoding search response: %v\n", err)
				return
			}

			for _, hit := range result.Hits.Hits {
				out <- hit.Source
			}

			scrollID = result.ScrollID
			if len(result.Hits.Hits) == 0 {
				return
			}

			scrollReq := esapi.ScrollRequest{
				ScrollID: scrollID,
				Scroll:   2 * time.Minute,
			}

			res, err = scrollReq.Do(es.ctx, es.client)
			if err != nil {
				fmt.Printf("Error scrolling search results: %v\n", err)
				return
			}
			defer res.Body.Close()

			if res.IsError() {
				body, _ := io.ReadAll(res.Body)
				fmt.Printf("scroll request error: %s\n", body)
				return
			}
		}
	}()
	return out
}

func (es *ElasticsearchBackend) GetAlertRecord(ruleName, alertID string) (backends.Record, error) {
	docID := fmt.Sprintf("%s_%s", ruleName, alertID)
	req := esapi.GetRequest{
		Index:      es.index,
		DocumentID: docID,
	}

	res, err := req.Do(es.ctx, es.client)
	if err != nil {
		return nil, fmt.Errorf("get request failed: %w", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		if res.StatusCode == 404 {
			return nil, nil
		}
		body, _ := io.ReadAll(res.Body)
		return nil, fmt.Errorf("get request error: %s", body)
	}

	var result struct {
		Source backends.Record `json:"_source"`
	}
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("error decoding get response: %w", err)
	}

	return result.Source, nil
}

func (es *ElasticsearchBackend) RuleNamesGenerator() <-chan string {
	out := make(chan string)
	go func() {
		defer close(out)

		query := map[string]interface{}{
			"aggs": map[string]interface{}{
				"unique_rule_names": map[string]interface{}{
					"terms": map[string]interface{}{
						"field": "RuleName.keyword",
						"size":  10000, // Retrieving up to 10000 unique rule names
					},
				},
			},
			"size": 0,
		}

		queryBytes, _ := json.Marshal(query)
		req := esapi.SearchRequest{
			Index: []string{es.index},
			Body:  bytes.NewReader(queryBytes),
		}

		res, err := req.Do(es.ctx, es.client)
		if err != nil {
			fmt.Printf("Error getting rule names: %v\n", err)
			return
		}
		defer res.Body.Close()

		if res.IsError() {
			body, _ := io.ReadAll(res.Body)
			fmt.Printf("search request error: %s\n", body)
			return
		}

		var result struct {
			Aggregations struct {
				UniqueRuleNames struct {
					Buckets []struct {
						Key string `json:"key"`
					} `json:"buckets"`
				} `json:"unique_rule_names"`
			} `json:"aggregations"`
		}
		if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
			fmt.Printf("Error decoding search response: %v\n", err)
			return
		}
		for _, bucket := range result.Aggregations.UniqueRuleNames.Buckets {
			out <- bucket.Key
		}
	}()
	return out
}

func (es *ElasticsearchBackend) MarkAsDispatched(alert *alerts.Alert) error {
	recordKey := alert.RecordKey()
	docID := fmt.Sprintf("%s_%s", recordKey["RuleName"], recordKey["AlertID"])

	update := map[string]interface{}{
		"doc": map[string]interface{}{
			"Attempts":   alert.Attempts,
			"Dispatched": alert.Dispatched.Format(helpers.DATETIME_FORMAT),
		},
	}
	updateBytes, err := json.Marshal(update)
	if err != nil {
		return fmt.Errorf("error marshalling update: %w", err)
	}

	req := esapi.UpdateRequest{
		Index:      es.index,
		DocumentID: docID,
		Body:       bytes.NewReader(updateBytes),
	}

	res, err := req.Do(es.ctx, es.client)
	if err != nil {
		return fmt.Errorf("update request failed: %w", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("update request error: %s", body)
	}

	return nil
}

func (es *ElasticsearchBackend) ToAlert(record backends.Record) (*alerts.Alert, error) {
	a := new(alerts.Alert)
	recordBytes, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal record: %w", err)
	}
	if err := json.Unmarshal(recordBytes, a); err != nil {
		return nil, fmt.Errorf("failed to unmarshal record to alert: %w", err)
	}

	if createdStr, ok := record["Created"].(string); ok {
		a.Created, err = time.Parse(helpers.DATETIME_FORMAT, createdStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse Created timestamp: %w", err)
		}
	}

	if dispatchedStr, ok := record["Dispatched"].(string); ok {
		dispatchedTime, err := time.Parse(helpers.DATETIME_FORMAT, dispatchedStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse Dispatched timestamp: %w", err)
		}
		a.Dispatched = dispatchedTime
	}

	if eventStr, ok := record["Event"].(string); ok {
		if err := json.Unmarshal([]byte(eventStr), &a.Event); err != nil {
			return nil, fmt.Errorf("failed to unmarshal Event JSON: %w", err)
		}
	}
	return a, nil
}

func (es *ElasticsearchBackend) ToRecord(alert *alerts.Alert) (backends.Record, error) {
	record := backends.Record{
		"RuleName":        alert.Rule.Name(), // Partition Key
		"AlertID":         alert.AlertID,     // Sort/Range Key
		"Attempts":        alert.Attempts,
		"Cluster":         alert.Cluster,
		"Created":         alert.Created.Format(helpers.DATETIME_FORMAT),
		"Dispatched":      alert.Dispatched.Format(helpers.DATETIME_FORMAT),
		"LogSource":       alert.LogSource,
		"LogType":         alert.LogType,
		"MergeByKeys":     alert.Rule.MergeByKeys(),
		"MergeWindowMins": alert.Rule.MergeWindowMins(),
		"Dispatchers":     alert.Rule.Dispatchers(),
		"OutputsSent":     alert.OutputsSent,
		"Formatters":      alert.Rule.Formatters(),
		"Event":           helpers.JsonCompact(alert.Event),
		"RuleDescription": alert.Rule.Description(),
		"SourceEntity":    alert.SourceEntity,
		"SourceService":   alert.SourceService,
		"Staged":          alert.Staged,
	}

	return record, nil
}
