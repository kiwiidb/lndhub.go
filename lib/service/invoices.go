package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/getAlby/lndhub.go/common"
	"github.com/getAlby/lndhub.go/db/models"
	"github.com/getAlby/lndhub.go/lnd"
	"github.com/getsentry/sentry-go"
	"github.com/labstack/gommon/random"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/schema"
)

type Route struct {
	TotalAmt  int64 `json:"total_amt"`
	TotalFees int64 `json:"total_fees"`
}

type SendPaymentResponse struct {
	PaymentPreimage    []byte `json:"payment_preimage,omitempty"`
	PaymentPreimageStr string
	PaymentError       string `json:"payment_error,omitempty"`
	PaymentHash        []byte `json:"payment_hash,omitempty"`
	PaymentHashStr     string
	PaymentRoute       *Route
	TransactionEntry   *models.TransactionEntry
	Invoice            *models.Invoice
}

func (svc *LndhubService) FindInvoiceByPaymentHash(ctx context.Context, userId int64, rHash string) (*models.Invoice, error) {
	var invoice models.Invoice

	err := svc.DB.NewSelect().Model(&invoice).Where("invoice.user_id = ? AND invoice.r_hash = ?", userId, rHash).Limit(1).Scan(ctx)
	if err != nil {
		return &invoice, err
	}
	return &invoice, nil
}

func (svc *LndhubService) SendInternalPayment(ctx context.Context, invoice *models.Invoice) (SendPaymentResponse, error) {
	sendPaymentResponse := SendPaymentResponse{}
	// find invoice
	var incomingInvoice models.Invoice
	err := svc.DB.NewSelect().Model(&incomingInvoice).Where("type = ? AND payment_request = ? AND state = ? ", common.InvoiceTypeIncoming, invoice.PaymentRequest, common.InvoiceStateOpen).Limit(1).Scan(ctx)
	if err != nil {
		// invoice not found or already settled
		// TODO: logging
		return sendPaymentResponse, err
	}
	// Get the user's current and incoming account for the transaction entry
	recipientCreditAccount, err := svc.AccountFor(ctx, common.AccountTypeCurrent, incomingInvoice.UserID)
	if err != nil {
		return sendPaymentResponse, err
	}
	recipientDebitAccount, err := svc.AccountFor(ctx, common.AccountTypeIncoming, incomingInvoice.UserID)
	if err != nil {
		return sendPaymentResponse, err
	}
	// create recipient entry
	recipientEntry := models.TransactionEntry{
		UserID:          incomingInvoice.UserID,
		InvoiceID:       incomingInvoice.ID,
		CreditAccountID: recipientCreditAccount.ID,
		DebitAccountID:  recipientDebitAccount.ID,
		Amount:          invoice.Amount,
	}
	_, err = svc.DB.NewInsert().Model(&recipientEntry).Exec(ctx)
	if err != nil {
		return sendPaymentResponse, err
	}

	// For internal invoices we know the preimage and we use that as a response
	// This allows wallets to get the correct preimage for a payment request even though NO lightning transaction was involved
	preimage, _ := hex.DecodeString(incomingInvoice.Preimage)
	sendPaymentResponse.PaymentPreimageStr = incomingInvoice.Preimage
	sendPaymentResponse.PaymentPreimage = preimage
	sendPaymentResponse.Invoice = invoice
	paymentHash, _ := hex.DecodeString(invoice.RHash)
	sendPaymentResponse.PaymentHashStr = invoice.RHash
	sendPaymentResponse.PaymentHash = paymentHash
	sendPaymentResponse.PaymentRoute = &Route{TotalAmt: invoice.Amount, TotalFees: 0}

	incomingInvoice.Internal = true // mark incoming invoice as internal, just for documentation/debugging
	incomingInvoice.State = common.InvoiceStateSettled
	incomingInvoice.SettledAt = schema.NullTime{Time: time.Now()}
	_, err = svc.DB.NewUpdate().Model(&incomingInvoice).WherePK().Exec(ctx)
	if err != nil {
		// could not save the invoice of the recipient
		return sendPaymentResponse, err
	}

	return sendPaymentResponse, nil
}

func (svc *LndhubService) SendPaymentSync(ctx context.Context, invoice *models.Invoice) (SendPaymentResponse, error) {
	sendPaymentResponse := SendPaymentResponse{}

	sendPaymentRequest, err := createLnRpcSendRequest(invoice)
	if err != nil {
		return sendPaymentResponse, err
	}

	// Execute the payment
	sendPaymentResult, err := svc.LndClient.SendPaymentSync(ctx, sendPaymentRequest)
	if err != nil {
		return sendPaymentResponse, err
	}

	// If there was a payment error we return an error
	if sendPaymentResult.GetPaymentError() != "" || sendPaymentResult.GetPaymentPreimage() == nil {
		return sendPaymentResponse, errors.New(sendPaymentResult.GetPaymentError())
	}

	preimage := sendPaymentResult.GetPaymentPreimage()
	sendPaymentResponse.PaymentPreimage = preimage
	sendPaymentResponse.PaymentPreimageStr = hex.EncodeToString(preimage[:])
	paymentHash := sendPaymentResult.GetPaymentHash()
	sendPaymentResponse.PaymentHash = paymentHash
	sendPaymentResponse.PaymentHashStr = hex.EncodeToString(paymentHash[:])
	sendPaymentResponse.PaymentRoute = &Route{TotalAmt: sendPaymentResult.PaymentRoute.TotalAmt, TotalFees: sendPaymentResult.PaymentRoute.TotalFees}
	return sendPaymentResponse, nil
}

func createLnRpcSendRequest(invoice *models.Invoice) (*lnrpc.SendRequest, error) {
	// TODO: set dynamic fee limit
	feeLimit := lnrpc.FeeLimit{
		//Limit: &lnrpc.FeeLimit_Percent{
		//	Percent: 2,
		//},
		Limit: &lnrpc.FeeLimit_Fixed{
			Fixed: 300,
		},
	}

	if !invoice.Keysend {
		return &lnrpc.SendRequest{
			PaymentRequest: invoice.PaymentRequest,
			Amt:            invoice.Amount,
			FeeLimit:       &feeLimit,
		}, nil
	}

	preImage := makePreimageHex()
	pHash := sha256.New()
	pHash.Write(preImage)
	// Prepare the LNRPC call
	//See: https://github.com/hsjoberg/blixt-wallet/blob/9fcc56a7dc25237bc14b85e6490adb9e044c009c/src/lndmobile/index.ts#L251-L270
	destBytes, err := hex.DecodeString(invoice.DestinationPubkeyHex)
	if err != nil {
		return nil, err
	}
	invoice.DestinationCustomRecords[KEYSEND_CUSTOM_RECORD] = preImage
	return &lnrpc.SendRequest{
		Dest:              destBytes,
		Amt:               invoice.Amount,
		PaymentHash:       pHash.Sum(nil),
		FeeLimit:          &feeLimit,
		DestFeatures:      []lnrpc.FeatureBit{lnrpc.FeatureBit_TLV_ONION_REQ},
		DestCustomRecords: invoice.DestinationCustomRecords,
	}, nil
}

func (svc *LndhubService) PayInvoice(ctx context.Context, invoice *models.Invoice) (*SendPaymentResponse, error) {
	userId := invoice.UserID

	// Get the user's current and outgoing account for the transaction entry
	debitAccount, err := svc.AccountFor(ctx, common.AccountTypeCurrent, userId)
	if err != nil {
		svc.Logger.Errorf("Could not find current account user_id:%v", invoice.UserID)
		return nil, err
	}
	creditAccount, err := svc.AccountFor(ctx, common.AccountTypeOutgoing, userId)
	if err != nil {
		svc.Logger.Errorf("Could not find outgoing account user_id:%v", invoice.UserID)
		return nil, err
	}

	entry := models.TransactionEntry{
		UserID:          userId,
		InvoiceID:       invoice.ID,
		CreditAccountID: creditAccount.ID,
		DebitAccountID:  debitAccount.ID,
		Amount:          invoice.Amount,
	}

	// The DB constraints make sure the user actually has enough balance for the transaction
	// If the user does not have enough balance this call fails
	_, err = svc.DB.NewInsert().Model(&entry).Exec(ctx)
	if err != nil {
		svc.Logger.Errorf("Could not insert transaction entry user_id:%v invoice_id:%v", invoice.UserID, invoice.ID)
		return nil, err
	}

	var paymentResponse SendPaymentResponse
	// Check the destination pubkey if it is an internal invoice and going to our node
	// Here we start using context.Background because we want to complete these calls
	// regardless of if the request's context is canceled or not.
	if svc.IdentityPubkey == invoice.DestinationPubkeyHex {
		paymentResponse, err = svc.SendInternalPayment(context.Background(), invoice)
		if err != nil {
			svc.HandleFailedPayment(context.Background(), invoice, entry, err)
			return nil, err
		}
	} else {
		paymentResponse, err = svc.SendPaymentSync(context.Background(), invoice)
		if err != nil {
			svc.HandleFailedPayment(context.Background(), invoice, entry, err)
			return nil, err
		}
	}

	paymentResponse.TransactionEntry = &entry

	// The payment was successful.
	// These changes to the invoice are persisted in the `HandleSuccessfulPayment` function
	invoice.Preimage = paymentResponse.PaymentPreimageStr
	invoice.Fee = paymentResponse.PaymentRoute.TotalFees
	err = svc.HandleSuccessfulPayment(context.Background(), invoice, entry)
	return &paymentResponse, err
}

func (svc *LndhubService) HandleFailedPayment(ctx context.Context, invoice *models.Invoice, entryToRevert models.TransactionEntry, failedPaymentError error) error {
	// add transaction entry with reverted credit/debit account id
	entry := models.TransactionEntry{
		UserID:          invoice.UserID,
		InvoiceID:       invoice.ID,
		CreditAccountID: entryToRevert.DebitAccountID,
		DebitAccountID:  entryToRevert.CreditAccountID,
		Amount:          invoice.Amount,
	}
	_, err := svc.DB.NewInsert().Model(&entry).Exec(ctx)
	if err != nil {
		sentry.CaptureException(err)
		svc.Logger.Errorf("Could not insert transaction entry user_id:%v invoice_id:%v", invoice.UserID, invoice.ID)
		return err
	}

	invoice.State = common.InvoiceStateError
	if failedPaymentError != nil {
		invoice.ErrorMessage = failedPaymentError.Error()
	}

	_, err = svc.DB.NewUpdate().Model(invoice).WherePK().Exec(ctx)
	if err != nil {
		sentry.CaptureException(err)
		svc.Logger.Errorf("Could not update failed payment invoice user_id:%v invoice_id:%v", invoice.UserID, invoice.ID)
	}
	return err
}

func (svc *LndhubService) HandleSuccessfulPayment(ctx context.Context, invoice *models.Invoice, parentEntry models.TransactionEntry) error {
	invoice.State = common.InvoiceStateSettled
	invoice.SettledAt = schema.NullTime{Time: time.Now()}

	_, err := svc.DB.NewUpdate().Model(invoice).WherePK().Exec(ctx)
	if err != nil {
		sentry.CaptureException(err)
		svc.Logger.Errorf("Could not update sucessful payment invoice user_id:%v invoice_id:%v", invoice.UserID, invoice.ID)
	}

	// Get the user's fee account for the transaction entry, current account is already there in parent entry
	feeAccount, err := svc.AccountFor(ctx, common.AccountTypeFees, invoice.UserID)
	if err != nil {
		svc.Logger.Errorf("Could not find fees account user_id:%v", invoice.UserID)
		return err
	}

	// add transaction entry for fee
	entry := models.TransactionEntry{
		UserID:          invoice.UserID,
		InvoiceID:       invoice.ID,
		CreditAccountID: feeAccount.ID,
		DebitAccountID:  parentEntry.DebitAccountID,
		Amount:          int64(invoice.Fee),
		ParentID:        parentEntry.ID,
	}
	_, err = svc.DB.NewInsert().Model(&entry).Exec(ctx)
	if err != nil {
		sentry.CaptureException(err)
		svc.Logger.Errorf("Could not insert fee transaction entry user_id:%v invoice_id:%v", invoice.UserID, invoice.ID)
		return err
	}

	userBalance, err := svc.CurrentUserBalance(ctx, entry.UserID)
	if err != nil {
		sentry.CaptureException(err)
		svc.Logger.Errorf("Could not fetch user balance user_id:%v invoice_id:%v", invoice.UserID, invoice.ID)
		return err
	}

	if userBalance < 0 {
		amountMsg := fmt.Sprintf("User balance is negative transaction_entry_id:%v user_id:%v amount:%v", entry.ID, entry.UserID, userBalance)
		svc.Logger.Info(amountMsg)
		sentry.CaptureMessage(amountMsg)
	}

	return nil
}

func (svc *LndhubService) AddOutgoingInvoice(ctx context.Context, userID int64, paymentRequest string, lnPayReq *lnd.LNPayReq) (*models.Invoice, error) {
	// Initialize new DB invoice
	invoice := models.Invoice{
		Type:                 common.InvoiceTypeOutgoing,
		UserID:               userID,
		PaymentRequest:       paymentRequest,
		RHash:                lnPayReq.PayReq.PaymentHash,
		Amount:               lnPayReq.PayReq.NumSatoshis,
		State:                common.InvoiceStateInitialized,
		DestinationPubkeyHex: lnPayReq.PayReq.Destination,
		DescriptionHash:      lnPayReq.PayReq.DescriptionHash,
		Memo:                 lnPayReq.PayReq.Description,
		Keysend:              lnPayReq.Keysend,
		ExpiresAt:            bun.NullTime{Time: time.Unix(lnPayReq.PayReq.Timestamp, 0).Add(time.Duration(lnPayReq.PayReq.Expiry) * time.Second)},
	}

	// Save invoice
	_, err := svc.DB.NewInsert().Model(&invoice).Exec(ctx)
	if err != nil {
		return nil, err
	}
	return &invoice, nil
}

func (svc *LndhubService) AddIncomingInvoice(ctx context.Context, userID int64, amount int64, memo, descriptionHashStr string) (*models.Invoice, error) {
	preimage := makePreimageHex()
	expiry := time.Hour * 24 // invoice expires in 24h
	// Initialize new DB invoice
	invoice := models.Invoice{
		Type:            common.InvoiceTypeIncoming,
		UserID:          userID,
		Amount:          amount,
		Memo:            memo,
		DescriptionHash: descriptionHashStr,
		State:           common.InvoiceStateInitialized,
		ExpiresAt:       bun.NullTime{Time: time.Now().Add(expiry)},
	}

	// Save invoice - we save the invoice early to have a record in case the LN call fails
	_, err := svc.DB.NewInsert().Model(&invoice).Exec(ctx)
	if err != nil {
		return nil, err
	}

	descriptionHash, err := hex.DecodeString(descriptionHashStr)
	if err != nil {
		return nil, err
	}
	// Initialize lnrpc invoice
	lnInvoice := lnrpc.Invoice{
		Memo:            memo,
		DescriptionHash: descriptionHash,
		Value:           amount,
		RPreimage:       preimage,
		Expiry:          int64(expiry.Seconds()),
	}
	// Call LND
	lnInvoiceResult, err := svc.LndClient.AddInvoice(ctx, &lnInvoice)
	if err != nil {
		return nil, err
	}

	// Update the DB invoice with the data from the LND gRPC call
	invoice.PaymentRequest = lnInvoiceResult.PaymentRequest
	invoice.RHash = hex.EncodeToString(lnInvoiceResult.RHash)
	invoice.Preimage = hex.EncodeToString(preimage)
	invoice.AddIndex = lnInvoiceResult.AddIndex
	invoice.DestinationPubkeyHex = svc.IdentityPubkey // Our node pubkey for incoming invoices
	invoice.State = common.InvoiceStateOpen

	_, err = svc.DB.NewUpdate().Model(&invoice).WherePK().Exec(ctx)
	if err != nil {
		return nil, err
	}

	return &invoice, nil
}

func (svc *LndhubService) DecodePaymentRequest(ctx context.Context, bolt11 string) (*lnrpc.PayReq, error) {
	return svc.LndClient.DecodeBolt11(ctx, bolt11)
}

const hexBytes = random.Hex

func makePreimageHex() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = hexBytes[rand.Intn(len(hexBytes))]
	}
	return b
}
