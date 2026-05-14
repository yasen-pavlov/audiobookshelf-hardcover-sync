package hardcover

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"strings"
	"testing"
	"time"

	"github.com/drallgood/audiobookshelf-hardcover-sync/internal/logger"
	"github.com/drallgood/audiobookshelf-hardcover-sync/internal/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGraphQLQuery_BookByASIN tests the GraphQL query functionality with a mock API server
// This is a unit test that doesn't require a real token
func TestGraphQLQuery_BookByASIN(t *testing.T) {
	// Initialize the logger
	logger.Setup(logger.Config{
		Level:      "debug",
		TimeFormat: "2006-01-02T15:04:05Z07:00",
	})

	// Get the logger
	log := logger.Get()

	// Create a mock server that returns a predefined response for ASIN queries
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the request body once
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err, "Error reading request body")
		
		// Create a new reader with the body content for parsing
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		
		// Check if it's a GetCurrentUserID query first
		if HandleGetCurrentUserIDQuery(t, w, r) {
			return
		}
		
		// Reuse the body content for our own parsing
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		
		// Parse the request body to check if it's the ASIN query
		var reqBody struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		
		// Decode the request body
		err = json.NewDecoder(r.Body).Decode(&reqBody)
		require.NoError(t, err, "Error decoding request body")
		
		// Check if this is our ASIN query
		if _, ok := reqBody.Variables["asin"]; ok {
			// Create a simple JSON response that matches the expected structure
			responseJSON := `{
				"data": {
					"books": [
						{
							"id": 123,
							"title": "Test Audiobook Title",
							"book_status_id": 1,
							"canonical_id": 456,
							"editions": [
								{
									"id": 789,
									"asin": "B00I8OW9R2",
									"isbn_13": null,
									"isbn_10": null,
									"reading_format_id": 2,
									"audio_seconds": 12345
								}
							]
						}
					]
				}
			}`
			
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(responseJSON))
			if err != nil {
				t.Fatalf("Failed to write response: %v", err)
			}
			return
		}
		
		// If we get here, it's an unknown query
		http.Error(w, "Unexpected query", http.StatusBadRequest)
	}))
	defer server.Close()

	// Create a client that uses our mock server
	client := CreateTestClient(server)
	client.logger = log

	// Define the query and variables
	query := `query BookByASIN($asin: String!) {
  books(
    where: { 
      editions: { 
        asin: { _eq: $asin }
        reading_format_id: { _eq: 2 }
      }
    }
    limit: 1
  ) {
    id
    title
    book_status_id
    canonical_id
    editions(
      where: { 
        asin: { _eq: $asin }
        reading_format_id: { _eq: 2 }
      }
    ) {
      id
      asin
      isbn_13
      isbn_10
      reading_format_id
      audio_seconds
    }
  }
}`

	variables := map[string]interface{}{
		"asin": "B00I8OW9R2",
	}

	// Define the response structure
	type BookEdition struct {
		ID              int     `json:"id"`
		ASIN            *string `json:"asin"`
		ISBN13          *string `json:"isbn_13"`
		ISBN10          *string `json:"isbn_10"`
		ReadingFormatID int     `json:"reading_format_id"`
		AudioSeconds    *int    `json:"audio_seconds"`
	}

	type Book struct {
		ID           int           `json:"id"`
		Title        string        `json:"title"`
		BookStatusID int           `json:"book_status_id"`
		CanonicalID  int           `json:"canonical_id"`
		Editions     []BookEdition `json:"editions"`
	}

	// Response structure is defined inline below

	// Define a response structure that exactly matches what the client will use for unmarshaling
	// The client will unmarshal only the "data" field from the response,
	// so our struct must match the structure inside the data field
	var response struct {
		Books []Book `json:"books"`
	}

	// Execute the query
	err := client.GraphQLQuery(context.Background(), query, variables, &response)
	require.NoError(t, err, "GraphQL query should not return an error")
	
	t.Logf("Response received: %+v", response)

	// Assert the response
	require.NotEmpty(t, response.Books, "Expected at least one book in the response")
	book := response.Books[0]
	assert.NotEmpty(t, book.Title, "Book title should not be empty")
	assert.Equal(t, "Test Audiobook Title", book.Title, "Book title should match the mock response")
	assert.NotEmpty(t, book.Editions, "Book should have at least one edition")

	edition := book.Editions[0]
	assert.Equal(t, 2, edition.ReadingFormatID, "Reading format ID should be 2 (audiobook)")
	assert.Equal(t, "B00I8OW9R2", *edition.ASIN, "ASIN should match the query parameter")
	assert.Equal(t, 12345, *edition.AudioSeconds, "Audio seconds should match the mock response")
}

func TestGraphQLQuery_RetriesOn429ThenSucceeds(t *testing.T) {
	logger.Setup(logger.Config{Level: "debug", Format: "json"})
	log := logger.Get()

	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"Throttled"}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"books":[{"id":1}]}}`))
	}))
	defer server.Close()

	client := CreateTestClient(server)
	client.logger = log
	client.maxRetries = 3
	client.retryDelay = 1 * time.Millisecond
	client.throttleBaseDelay = 1 * time.Millisecond
	client.rateLimiter = util.NewRateLimiter(time.Nanosecond, 100, 100, log)

	var response struct {
		Books []struct {
			ID int `json:"id"`
		} `json:"books"`
	}

	err := client.GraphQLQuery(context.Background(), `query RetryTest { books { id } }`, nil, &response)
	require.NoError(t, err)
	require.Len(t, response.Books, 1)
	assert.Equal(t, 3, int(atomic.LoadInt32(&attempts)))
	assert.Equal(t, 1, response.Books[0].ID)
}

func TestGraphQLQuery_FailsFastOn400(t *testing.T) {
	logger.Setup(logger.Config{Level: "debug", Format: "json"})
	log := logger.Get()

	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer server.Close()

	client := CreateTestClient(server)
	client.logger = log
	client.maxRetries = 3
	client.retryDelay = 1 * time.Millisecond
	client.throttleBaseDelay = 1 * time.Millisecond
	client.rateLimiter = util.NewRateLimiter(time.Nanosecond, 100, 100, log)

	var response struct {
		Books []struct {
			ID int `json:"id"`
		} `json:"books"`
	}

	err := client.GraphQLQuery(context.Background(), `query FailFastTest { books { id } }`, nil, &response)
	require.Error(t, err)
	assert.Equal(t, 1, int(atomic.LoadInt32(&attempts)))
	assert.Contains(t, err.Error(), "non-retryable HTTP error")
}

// TestGraphQLQuery_429RetriesPastBaseLimitWithThrottledBody verifies that 429
// responses are granted extra retry budget on top of c.maxRetries. With
// maxRetries=3 the call would normally bail after 4 attempts; the throttled
// body should extend the budget so a 5-times-then-200 server still succeeds.
func TestGraphQLQuery_429RetriesPastBaseLimitWithThrottledBody(t *testing.T) {
	logger.Setup(logger.Config{Level: "debug", Format: "json"})
	log := logger.Get()

	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) <= 5 {
			// No Retry-After header — exercises the throttled-body branch
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"Throttled"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"books":[{"id":7}]}}`))
	}))
	defer server.Close()

	client := CreateTestClient(server)
	client.logger = log
	client.maxRetries = 3
	client.retryDelay = 1 * time.Millisecond
	client.throttleBaseDelay = 1 * time.Millisecond
	client.rateLimiter = util.NewRateLimiter(time.Nanosecond, 100, 100, log)

	var response struct {
		Books []struct {
			ID int `json:"id"`
		} `json:"books"`
	}

	err := client.GraphQLQuery(context.Background(), `query Retry429 { books { id } }`, nil, &response)
	require.NoError(t, err)
	assert.Equal(t, 6, int(atomic.LoadInt32(&attempts)))
	require.Len(t, response.Books, 1)
	assert.Equal(t, 7, response.Books[0].ID)
}

// TestGraphQLQuery_429BudgetExhausts verifies that we don't retry forever on
// persistent 429s — once the 429-specific budget is spent the call fails.
func TestGraphQLQuery_429BudgetExhausts(t *testing.T) {
	logger.Setup(logger.Config{Level: "debug", Format: "json"})
	log := logger.Get()

	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"Throttled"}`))
	}))
	defer server.Close()

	client := CreateTestClient(server)
	client.logger = log
	client.maxRetries = 3
	client.retryDelay = 1 * time.Millisecond
	client.throttleBaseDelay = 1 * time.Millisecond
	client.rateLimiter = util.NewRateLimiter(time.Nanosecond, 100, 100, log)

	var response struct{}
	err := client.GraphQLQuery(context.Background(), `query Retry429Exhaust { books { id } }`, nil, &response)
	require.Error(t, err)
	// maxRetries(3) + 1 + max429(5) = 9 attempts total
	assert.Equal(t, 9, int(atomic.LoadInt32(&attempts)))
	assert.Contains(t, err.Error(), "429")
}

// TestGraphQLQuery_429HonoursRetryAfter verifies that the Retry-After header
// is taken as a lower bound on the next inter-attempt delay.
func TestGraphQLQuery_429HonoursRetryAfter(t *testing.T) {
	logger.Setup(logger.Config{Level: "debug", Format: "json"})
	log := logger.Get()

	var (
		attempts        int32
		firstHitAt      time.Time
		secondHitAt     time.Time
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&attempts, 1) {
		case 1:
			firstHitAt = time.Now()
			w.Header().Set("Retry-After", "1") // 1 second
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"Throttled"}`))
		default:
			secondHitAt = time.Now()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"books":[{"id":42}]}}`))
		}
	}))
	defer server.Close()

	client := CreateTestClient(server)
	client.logger = log
	client.maxRetries = 3
	client.retryDelay = 1 * time.Millisecond
	client.throttleBaseDelay = 1 * time.Millisecond
	client.rateLimiter = util.NewRateLimiter(time.Nanosecond, 100, 100, log)

	var response struct {
		Books []struct {
			ID int `json:"id"`
		} `json:"books"`
	}
	err := client.GraphQLQuery(context.Background(), `query RetryAfterTest { books { id } }`, nil, &response)
	require.NoError(t, err)
	require.Len(t, response.Books, 1)

	// Inter-attempt delay should be at least the Retry-After value.
	require.False(t, firstHitAt.IsZero(), "server didn't record first hit")
	require.False(t, secondHitAt.IsZero(), "server didn't record second hit")
	gap := secondHitAt.Sub(firstHitAt)
	assert.GreaterOrEqual(t, gap, 900*time.Millisecond,
		"expected at least ~1s between attempts due to Retry-After, got %s", gap)
}

func TestIsThrottledBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"empty", "", false},
		{"plain throttled", `{"error":"Throttled"}`, true},
		{"capital throttled", `{"error":"THROTTLED"}`, true},
		{"throttled in different key", `{"detail":"Throttled"}`, true},
		{"unrelated 429 body", `{"error":"rate exceeded"}`, false},
		{"unquoted throttled mention", `{"detail":"request was throttled by upstream"}`, false},
		{"valid response", `{"data":{"books":[]}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isThrottledBody([]byte(tc.body))
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestComputeBackoffDelay(t *testing.T) {
	base := 100 * time.Millisecond

	// n < 1 returns base
	assert.Equal(t, base, computeBackoffDelay(base, 0))
	assert.Equal(t, base, computeBackoffDelay(base, -1))

	// Exponential growth, within ±25% jitter band.
	for n := 1; n <= 5; n++ {
		expected := base * (1 << uint(n-1))
		got := computeBackoffDelay(base, n)
		minOK := time.Duration(float64(expected) * 0.75)
		maxOK := time.Duration(float64(expected) * 1.25)
		assert.GreaterOrEqual(t, got, minOK, "n=%d expected >= %s, got %s", n, minOK, got)
		assert.LessOrEqual(t, got, maxOK, "n=%d expected <= %s, got %s", n, maxOK, got)
	}

	// Caps at MaxRetryDelay regardless of n.
	assert.LessOrEqual(t, computeBackoffDelay(base, 30), MaxRetryDelay)

	// Zero base produces zero delay.
	assert.Equal(t, time.Duration(0), computeBackoffDelay(0, 5))
}
