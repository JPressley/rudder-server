package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	uuid "github.com/satori/go.uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	backendconfig "github.com/rudderlabs/rudder-server/config/backend-config"
	"github.com/rudderlabs/rudder-server/jobsdb"
	"github.com/rudderlabs/rudder-server/mocks"
	"github.com/rudderlabs/rudder-server/services/stats"
	"github.com/rudderlabs/rudder-server/utils"
	"github.com/rudderlabs/rudder-server/utils/logger"
	"github.com/rudderlabs/rudder-server/utils/misc"
	testutils "github.com/rudderlabs/rudder-server/utils/tests"
)

const (
	WriteKeyEnabled   = "enabled-write-key"
	WriteKeyDisabled  = "disabled-write-key"
	WriteKeyInvalid   = "invalid-write-key"
	WriteKeyEmpty     = ""
	SourceIDEnabled   = "enabled-source"
	SourceIDDisabled  = "disabled-source"
	TestRemoteAddress = "test.com"
)

// This configuration is assumed by all gateway tests and, is returned on Subscribe of mocked backend config
var sampleBackendConfig = backendconfig.SourcesT{
	Sources: []backendconfig.SourceT{
		backendconfig.SourceT{
			ID:       SourceIDDisabled,
			WriteKey: WriteKeyDisabled,
			Enabled:  false,
		},
		backendconfig.SourceT{
			ID:       SourceIDEnabled,
			WriteKey: WriteKeyEnabled,
			Enabled:  true,
		},
	},
}

type context struct {
	asyncHelper testutils.AsyncTestHelper

	mockCtrl          *gomock.Controller
	mockJobsDB        *mocks.MockJobsDB
	mockBackendConfig *mocks.MockBackendConfig
	mockRateLimiter   *mocks.MockRateLimiter
	mockStats         *mocks.MockStats

	mockStatGatewayResponseTime *mocks.MockRudderStats
	mockStatGatewayBatchSize    *mocks.MockRudderStats
	mockStatGatewayBatchTime    *mocks.MockRudderStats
}

// Initiaze mocks and common expectations
func (c *context) Setup() {
	c.mockCtrl = gomock.NewController(GinkgoT())
	c.mockJobsDB = mocks.NewMockJobsDB(c.mockCtrl)
	c.mockBackendConfig = mocks.NewMockBackendConfig(c.mockCtrl)
	c.mockRateLimiter = mocks.NewMockRateLimiter(c.mockCtrl)
	c.mockStats = mocks.NewMockStats(c.mockCtrl)

	c.mockStatGatewayResponseTime = mocks.NewMockRudderStats(c.mockCtrl)
	c.mockStatGatewayBatchSize = mocks.NewMockRudderStats(c.mockCtrl)
	c.mockStatGatewayBatchTime = mocks.NewMockRudderStats(c.mockCtrl)

	// During Setup, gateway always creates the following stats
	c.mockStats.EXPECT().NewStat("gateway.response_time", stats.TimerType).Return(c.mockStatGatewayResponseTime).Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())
	c.mockStats.EXPECT().NewStat("gateway.batch_size", stats.CountType).Return(c.mockStatGatewayBatchSize).Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())
	c.mockStats.EXPECT().NewStat("gateway.batch_time", stats.TimerType).Return(c.mockStatGatewayBatchTime).Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())

	// During Setup, gateway subscribes to backend config and waits until it is received.
	c.mockBackendConfig.EXPECT().WaitForConfig().Return().Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())
	c.mockBackendConfig.EXPECT().Subscribe(gomock.Any(), backendconfig.TopicProcessConfig).
		Do(func(channel chan utils.DataEvent, topic string) {
			// on Subscribe, emulate a backend configuration event
			go func() { channel <- utils.DataEvent{Data: sampleBackendConfig, Topic: topic} }()
		}).
		Do(c.asyncHelper.ExpectAndNotifyCallback()).
		Return().Times(1)
}

func (c *context) Finish() {
	c.asyncHelper.WaitWithTimeout(time.Minute)
	c.mockCtrl.Finish()
}

// helper function to add expectations about a specific writeKey stat. Returns gomock.Call of RudderStats Count()
func (c *context) expectWriteKeyStat(name string, writeKey string, count int) *gomock.Call {
	mockStat := mocks.NewMockRudderStats(c.mockCtrl)

	c.mockStats.EXPECT().NewWriteKeyStat(name, stats.CountType, writeKey).
		Return(mockStat).Times(1).
		Do(c.asyncHelper.ExpectAndNotifyCallback())

	return mockStat.EXPECT().Count(count).
		Times(1).
		Do(c.asyncHelper.ExpectAndNotifyCallback())
}

var _ = Describe("Gateway", func() {
	var c *context

	BeforeEach(func() {
		c = &context{}
		c.Setup()

		// setup static requirements of dependencies
		logger.Setup()
		stats.Setup()

		// setup common environment, override in BeforeEach when required
		SetEnableRateLimit(false)
		SetEnableDedup(false)
	})

	AfterEach(func() {
		c.Finish()
	})

	Context("Initialization", func() {
		gateway := &HandleT{}
		var clearDB = false

		It("should wait for backend config", func() {
			gateway.Setup(c.mockBackendConfig, c.mockJobsDB, nil, c.mockStats, &clearDB)
		})
	})

	Context("Valid requests", func() {
		var (
			gateway      = &HandleT{}
			clearDB bool = false
		)

		BeforeEach(func() {
			gateway.Setup(c.mockBackendConfig, c.mockJobsDB, nil, c.mockStats, &clearDB)
		})

		// common tests for all web handlers
		assertSingleMessageHandler := func(handlerType string, handler http.HandlerFunc) {
			It("should accept valid requests, and store to jobsdb", func() {
				validBody := `{"data":{"string":"valid-json","nested":{"child":1}}}`
				validBodyJSON := json.RawMessage(validBody)

				c.mockStatGatewayBatchSize.EXPECT().Count(1).
					Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())

				callStart := c.mockStatGatewayBatchTime.EXPECT().Start().Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())
				c.mockStatGatewayBatchTime.EXPECT().End().After(callStart).Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())

				c.expectWriteKeyStat("gateway.write_key_requests", WriteKeyEnabled, 1)
				c.expectWriteKeyStat("gateway.write_key_successful_requests", WriteKeyEnabled, 1)
				c.expectWriteKeyStat("gateway.write_key_events", WriteKeyEnabled, 0)
				c.expectWriteKeyStat("gateway.write_key_successful_events", WriteKeyEnabled, 0)

				c.mockJobsDB.
					EXPECT().Store(gomock.Any()).
					DoAndReturn(func(jobs []*jobsdb.JobT) map[uuid.UUID]string {
						for _, job := range jobs {
							Expect(misc.IsValidUUID(job.UUID.String())).To(Equal(true))
							Expect(job.Parameters).To(Equal(json.RawMessage(fmt.Sprintf(`{"source_id": "%v"}`, SourceIDEnabled))))
							Expect(job.CustomVal).To(Equal(CustomVal))

							responseData := []byte(job.EventPayload)
							receivedAt := gjson.GetBytes(responseData, "receivedAt")
							writeKey := gjson.GetBytes(responseData, "writeKey")
							requestIP := gjson.GetBytes(responseData, "requestIP")
							batch := gjson.GetBytes(responseData, "batch")
							payload := gjson.GetBytes(responseData, "batch.0")
							messageID := payload.Get("messageId")
							anonymousID := payload.Get("anonymousId")
							messageType := payload.Get("type")

							strippedPayload, _ := sjson.Delete(payload.String(), "messageId")
							strippedPayload, _ = sjson.Delete(strippedPayload, "anonymousId")
							strippedPayload, _ = sjson.Delete(strippedPayload, "type")

							// Assertions regarding response metadata
							Expect(time.Parse(misc.RFC3339Milli, receivedAt.String())).To(BeTemporally("~", time.Now(), 10*time.Millisecond))
							Expect(writeKey.String()).To(Equal(WriteKeyEnabled))
							Expect(requestIP.String()).To(Equal(TestRemoteAddress))

							// Assertions regarding batch
							Expect(batch.Array()).To(HaveLen(1))

							// Assertions regarding batch message
							Expect(messageID.Exists()).To(BeTrue())
							Expect(messageID.String()).To(testutils.BeValidUUID())
							Expect(anonymousID.Exists()).To(BeTrue())
							Expect(anonymousID.String()).To(testutils.BeValidUUID())
							Expect(messageType.Exists()).To(BeTrue())
							Expect(messageType.String()).To(Equal(handlerType))
							Expect(strippedPayload).To(MatchJSON(validBodyJSON))
						}
						c.asyncHelper.ExpectAndNotifyCallback()()

						return jobsToEmptyErrors(jobs)
					}).
					Times(1)

				expectHandlerResponse(handler, authorizedRequest(WriteKeyEnabled, bytes.NewBufferString(validBody)), 200, "OK")
			})
		}

		for handlerType, handler := range allHandlers(gateway) {
			if handlerType != "batch" {
				assertSingleMessageHandler(handlerType, handler)
			}
		}
	})

	Context("Rate limits", func() {
		var (
			gateway      = &HandleT{}
			clearDB bool = false
		)

		BeforeEach(func() {
			SetEnableRateLimit(true)
			gateway.Setup(c.mockBackendConfig, c.mockJobsDB, c.mockRateLimiter, c.mockStats, &clearDB)
		})

		It("should store messages successfuly if rate limit is not reached for workspace", func() {
			workspaceID := "some-workspace-id"

			callStart := c.mockStatGatewayBatchTime.EXPECT().Start().Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())
			c.mockBackendConfig.EXPECT().GetWorkspaceIDForWriteKey(WriteKeyEnabled).Return(workspaceID).AnyTimes().Do(c.asyncHelper.ExpectAndNotifyCallback())
			c.mockRateLimiter.EXPECT().LimitReached(workspaceID).Return(false).Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())
			callStore := c.mockJobsDB.EXPECT().Store(gomock.Any()).DoAndReturn(jobsToEmptyErrors).Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())
			callEnd := c.mockStatGatewayBatchTime.EXPECT().End().After(callStart).After(callStore).Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())
			c.mockStatGatewayBatchSize.EXPECT().Count(1).After(callEnd).Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())

			c.expectWriteKeyStat("gateway.write_key_requests", WriteKeyEnabled, 1)
			c.expectWriteKeyStat("gateway.write_key_successful_requests", WriteKeyEnabled, 1)
			c.expectWriteKeyStat("gateway.write_key_events", WriteKeyEnabled, 0)
			c.expectWriteKeyStat("gateway.write_key_successful_events", WriteKeyEnabled, 0)

			expectHandlerResponse(gateway.webAliasHandler, authorizedRequest(WriteKeyEnabled, bytes.NewBufferString("{}")), 200, "OK")
		})

		It("should reject messages if rate limit is reached for workspace", func() {
			workspaceID := "some-workspace-id"
			var emptyJobsList []*jobsdb.JobT

			callStart := c.mockStatGatewayBatchTime.EXPECT().Start().Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())
			c.mockBackendConfig.EXPECT().GetWorkspaceIDForWriteKey(WriteKeyEnabled).Return(workspaceID).AnyTimes().Do(c.asyncHelper.ExpectAndNotifyCallback())
			c.mockRateLimiter.EXPECT().LimitReached(workspaceID).Return(true).Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())
			callStore := c.mockJobsDB.EXPECT().Store(emptyJobsList).DoAndReturn(jobsToEmptyErrors).Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())
			callEnd := c.mockStatGatewayBatchTime.EXPECT().End().After(callStart).After(callStore).Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())
			c.mockStatGatewayBatchSize.EXPECT().Count(1).After(callEnd).Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())

			c.expectWriteKeyStat("gateway.write_key_requests", WriteKeyEnabled, 1)
			c.expectWriteKeyStat("gateway.work_space_dropped_requests", workspaceID, 1)

			expectHandlerResponse(gateway.webAliasHandler, authorizedRequest(WriteKeyEnabled, bytes.NewBufferString("{}")), 400, TooManyRequests+"\n")
		})
	})

	Context("Invalid requests", func() {
		var (
			gateway      = &HandleT{}
			clearDB bool = false
		)

		BeforeEach(func() {
			// all of these request errors will cause JobsDB.Store to be called with an empty job list
			var emptyJobsList []*jobsdb.JobT

			callStart := c.mockStatGatewayBatchTime.EXPECT().Start().Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())

			callStore := c.mockJobsDB.
				EXPECT().Store(emptyJobsList).
				Do(c.asyncHelper.ExpectAndNotifyCallback()).
				Return(jobsToEmptyErrors(emptyJobsList)).
				Times(1)

			c.mockStatGatewayBatchTime.EXPECT().End().After(callStart).After(callStore).Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())

			gateway.Setup(c.mockBackendConfig, c.mockJobsDB, nil, c.mockStats, &clearDB)
		})

		// common tests for all web handlers
		assertHandler := func(handler http.HandlerFunc) {
			It("should reject requests without Authorization header", func() {
				c.mockStatGatewayBatchSize.EXPECT().Count(1).
					Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())

				c.expectWriteKeyStat("gateway.write_key_requests", "", 1)
				c.expectWriteKeyStat("gateway.write_key_failed_requests", "noWriteKey", 1)

				expectHandlerResponse(handler, unauthorizedRequest(nil), 400, NoWriteKeyInBasicAuth+"\n")
			})

			It("should reject requests without username in Authorization header", func() {
				c.mockStatGatewayBatchSize.EXPECT().Count(1).
					Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())

				c.expectWriteKeyStat("gateway.write_key_requests", "", 1)
				c.expectWriteKeyStat("gateway.write_key_failed_requests", "noWriteKey", 1)

				expectHandlerResponse(handler, authorizedRequest(WriteKeyEmpty, nil), 400, NoWriteKeyInBasicAuth+"\n")
			})

			It("should reject requests without request body", func() {
				c.mockStatGatewayBatchSize.EXPECT().Count(1).
					Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())

				c.expectWriteKeyStat("gateway.write_key_requests", WriteKeyInvalid, 1)

				expectHandlerResponse(handler, authorizedRequest(WriteKeyInvalid, nil), 400, RequestBodyNil+"\n")
			})

			It("should reject requests without valid json in request body", func() {
				invalidBody := "not-a-valid-json"

				c.mockStatGatewayBatchSize.EXPECT().Count(1).
					Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())

				c.expectWriteKeyStat("gateway.write_key_requests", WriteKeyInvalid, 1)
				c.expectWriteKeyStat("gateway.write_key_failed_requests", WriteKeyInvalid, 1)

				expectHandlerResponse(handler, authorizedRequest(WriteKeyInvalid, bytes.NewBufferString(invalidBody)), 400, InvalidJSON+"\n")
			})

			It("should reject requests with request bodies larger than configured limit", func() {
				data := make([]byte, gateway.MaxReqSize())
				for i := range data {
					data[i] = 'a'
				}
				body := fmt.Sprintf(`{"data":"%s"}`, string(data))

				c.mockStatGatewayBatchSize.EXPECT().Count(1).
					Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())

				c.expectWriteKeyStat("gateway.write_key_requests", WriteKeyInvalid, 1)
				c.expectWriteKeyStat("gateway.write_key_failed_requests", WriteKeyInvalid, 1)
				c.expectWriteKeyStat("gateway.write_key_events", WriteKeyInvalid, 0)
				c.expectWriteKeyStat("gateway.write_key_failed_events", WriteKeyInvalid, 0)

				expectHandlerResponse(handler, authorizedRequest(WriteKeyInvalid, bytes.NewBufferString(body)), 400, RequestBodyTooLarge+"\n")
			})

			It("should reject requests with invalid write keys", func() {
				validBody := `{"data":"valid-json"}`

				c.mockStatGatewayBatchSize.EXPECT().Count(1).
					Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())

				c.expectWriteKeyStat("gateway.write_key_requests", WriteKeyInvalid, 1)
				c.expectWriteKeyStat("gateway.write_key_failed_requests", WriteKeyInvalid, 1)
				c.expectWriteKeyStat("gateway.write_key_events", WriteKeyInvalid, 0)
				c.expectWriteKeyStat("gateway.write_key_failed_events", WriteKeyInvalid, 0)

				expectHandlerResponse(handler, authorizedRequest(WriteKeyInvalid, bytes.NewBufferString(validBody)), 400, InvalidWriteKey+"\n")
			})

			It("should reject requests with disabled write keys", func() {
				validBody := `{"data":"valid-json"}`

				c.mockStatGatewayBatchSize.EXPECT().Count(1).
					Times(1).Do(c.asyncHelper.ExpectAndNotifyCallback())

				c.expectWriteKeyStat("gateway.write_key_requests", WriteKeyDisabled, 1)
				c.expectWriteKeyStat("gateway.write_key_failed_requests", WriteKeyDisabled, 1)
				c.expectWriteKeyStat("gateway.write_key_events", WriteKeyDisabled, 0)
				c.expectWriteKeyStat("gateway.write_key_failed_events", WriteKeyDisabled, 0)

				expectHandlerResponse(handler, authorizedRequest(WriteKeyDisabled, bytes.NewBufferString(validBody)), 400, InvalidWriteKey+"\n")
			})
		}

		for _, handler := range allHandlers(gateway) {
			assertHandler(handler)
		}
	})
})

func unauthorizedRequest(body io.Reader) *http.Request {
	req, err := http.NewRequest("GET", "", body)
	if err != nil {
		panic(err)
	}

	return req
}

func authorizedRequest(username string, body io.Reader) *http.Request {
	req := unauthorizedRequest(body)

	basicAuth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:password-should-be-ignored", username)))

	req.Header.Set("Authorization", fmt.Sprintf("Basic %s", basicAuth))
	req.RemoteAddr = TestRemoteAddress
	return req
}

func expectHandlerResponse(handler http.HandlerFunc, req *http.Request, responseStatus int, responseBody string) {
	testutils.RunTestWithTimeout(func() {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		bodyBytes, _ := ioutil.ReadAll(rr.Body)
		body := string(bodyBytes)
		Expect(rr.Result().StatusCode).To(Equal(responseStatus))
		Expect(body).To(Equal(responseBody))
	}, time.Minute)
}

func allHandlers(gateway *HandleT) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"alias":    gateway.webAliasHandler,
		"batch":    gateway.webBatchHandler,
		"group":    gateway.webGroupHandler,
		"identify": gateway.webIdentifyHandler,
		"page":     gateway.webPageHandler,
		"screen":   gateway.webScreenHandler,
		"track":    gateway.webTrackHandler,
	}
}

// converts a job list to a map of empty errors, to emulate a successful jobsdb.Store response
func jobsToEmptyErrors(jobs []*jobsdb.JobT) map[uuid.UUID]string {
	var result = make(map[uuid.UUID]string)
	for _, job := range jobs {
		result[job.UUID] = ""
	}
	return result
}