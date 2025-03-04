package integration_tests

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/getAlby/lndhub.go/controllers"
	"github.com/getAlby/lndhub.go/lib"
	"github.com/getAlby/lndhub.go/lib/responses"
	"github.com/getAlby/lndhub.go/lib/service"
	"github.com/getAlby/lndhub.go/lib/tokens"
	"github.com/getAlby/lndhub.go/lnd"
	"github.com/go-playground/validator/v10"
	"github.com/labstack/echo/v4"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
)

type CheckPaymentTestSuite struct {
	TestSuite
	fundingClient            *lnd.LNDWrapper
	service                  *service.LndhubService
	userLogin                controllers.CreateUserResponseBody
	userToken                string
	invoiceUpdateSubCancelFn context.CancelFunc
}

func (suite *CheckPaymentTestSuite) SetupSuite() {
	lndClient, err := lnd.NewLNDclient(lnd.LNDoptions{
		Address:     lnd2RegtestAddress,
		MacaroonHex: lnd2RegtestMacaroonHex,
	})
	if err != nil {
		log.Fatalf("Error setting up funding client: %v", err)
	}
	suite.fundingClient = lndClient

	svc, err := LndHubTestServiceInit(nil)
	if err != nil {
		log.Fatalf("Error initializing test service: %v", err)
	}
	users, userTokens, err := createUsers(svc, 1)
	if err != nil {
		log.Fatalf("Error creating test users: %v", err)
	}
	// Subscribe to LND invoice updates in the background
	// store cancel func to be called in tear down suite
	ctx, cancel := context.WithCancel(context.Background())
	suite.invoiceUpdateSubCancelFn = cancel
	go svc.InvoiceUpdateSubscription(ctx)
	suite.service = svc
	e := echo.New()

	e.HTTPErrorHandler = responses.HTTPErrorHandler
	e.Validator = &lib.CustomValidator{Validator: validator.New()}
	suite.echo = e
	assert.Equal(suite.T(), 1, len(users))
	assert.Equal(suite.T(), 1, len(userTokens))
	suite.userLogin = users[0]
	suite.userToken = userTokens[0]
	suite.echo.Use(tokens.Middleware([]byte(suite.service.Config.JWTSecret)))
	suite.echo.POST("/addinvoice", controllers.NewAddInvoiceController(suite.service).AddInvoice)
	suite.echo.POST("/payinvoice", controllers.NewPayInvoiceController(suite.service).PayInvoice)
	suite.echo.GET("/checkpayment/:payment_hash", controllers.NewCheckPaymentController(suite.service).CheckPayment)
}

func (suite *CheckPaymentTestSuite) TestCheckPaymentNotFound() {
	dummyRHash := "12345"
	req := httptest.NewRequest(http.MethodGet, "/checkpayment/"+dummyRHash, nil)
	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", suite.userToken))
	rec := httptest.NewRecorder()
	suite.echo.ServeHTTP(rec, req)
	errorResponse := &responses.ErrorResponse{}
	assert.Equal(suite.T(), http.StatusBadRequest, rec.Code)
	assert.NoError(suite.T(), json.NewDecoder(rec.Body).Decode(errorResponse))
}

func (suite *CheckPaymentTestSuite) TestCheckPaymentProperIsPaidResponse() {
	// create incoming invoice and fund account
	invoice := suite.createAddInvoiceReq(1000, "integration test check payments for user", suite.userToken)
	sendPaymentRequest := lnrpc.SendRequest{
		PaymentRequest: invoice.PayReq,
		FeeLimit:       nil,
	}
	_, err := suite.fundingClient.SendPaymentSync(context.Background(), &sendPaymentRequest)
	assert.NoError(suite.T(), err)

	// wait a bit for the callback event to hit
	time.Sleep(100 * time.Millisecond)
	// create invoice
	invoice = suite.createAddInvoiceReq(500, "integration test check payments for user", suite.userToken)
	// pay invoice, this will create outgoing invoice and settle it

	// check payment not paid
	req := httptest.NewRequest(http.MethodGet, "/checkpayment/"+invoice.RHash, nil)
	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", suite.userToken))
	rec := httptest.NewRecorder()
	suite.echo.ServeHTTP(rec, req)
	checkPaymentResponse := &controllers.CheckPaymentResponseBody{}
	assert.Equal(suite.T(), http.StatusOK, rec.Code)
	assert.NoError(suite.T(), json.NewDecoder(rec.Body).Decode(checkPaymentResponse))
	assert.False(suite.T(), checkPaymentResponse.IsPaid)

	// pay external from user
	payResponse := suite.createPayInvoiceReq(invoice.PaymentRequest, suite.userToken)
	assert.NotEmpty(suite.T(), payResponse.PaymentPreimage)

	// check payment is paid
	req = httptest.NewRequest(http.MethodGet, "/checkpayment/"+invoice.RHash, nil)
	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", suite.userToken))
	rec = httptest.NewRecorder()
	suite.echo.ServeHTTP(rec, req)
	checkPaymentResponse = &controllers.CheckPaymentResponseBody{}
	assert.Equal(suite.T(), http.StatusOK, rec.Code)
	assert.NoError(suite.T(), json.NewDecoder(rec.Body).Decode(checkPaymentResponse))
	assert.True(suite.T(), checkPaymentResponse.IsPaid)
}

func (suite *CheckPaymentTestSuite) TearDownSuite() {
	suite.invoiceUpdateSubCancelFn()
}

func TestCheckPaymentSuite(t *testing.T) {
	suite.Run(t, new(CheckPaymentTestSuite))
}
