package controllers

import (
	"net/http"

	"github.com/getAlby/lndhub.go/common"
	"github.com/getAlby/lndhub.go/db/models"
	"github.com/getAlby/lndhub.go/lib/service"
	"github.com/getAlby/lndhub.go/lib/tokens"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
)

// GetTXSController : GetTXSController struct
type InvoiceStreamController struct {
	svc *service.LndhubService
}

func NewInvoiceStreamController(svc *service.LndhubService) *InvoiceStreamController {
	return &InvoiceStreamController{svc: svc}
}

// Stream invoices streams incoming payments to the client
func (controller *InvoiceStreamController) StreamInvoices(c echo.Context) error {
	userId, err := tokens.ParseToken(controller.svc.Config.JWTSecret, (c.QueryParam("token")))
	if err != nil {
		return err
	}
	invoiceChan := make(chan models.Invoice)
	controller.svc.InvoiceSubscribers[userId] = invoiceChan
	ctx := c.Request().Context()
	upgrader := websocket.Upgrader{}
	upgrader.CheckOrigin = func(r *http.Request) bool { return true }
	ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return err
	}
	defer ws.Close()
SocketLoop:
	for {
		select {
		case <-ctx.Done():
			break SocketLoop
		case invoice := <-invoiceChan:
			err := ws.WriteJSON(
				&IncomingInvoice{
					PaymentHash:    invoice.RHash,
					PaymentRequest: invoice.PaymentRequest,
					Description:    invoice.Memo,
					PayReq:         invoice.PaymentRequest,
					Timestamp:      invoice.CreatedAt.Unix(),
					Type:           common.InvoiceTypeUser,
					Amount:         invoice.Amount,
					IsPaid:         invoice.State == common.InvoiceStateSettled,
				})
			if err != nil {
				controller.svc.Logger.Error(err)
				break SocketLoop
			}
		}
	}
	return nil
}
