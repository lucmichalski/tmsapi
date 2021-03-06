package stripe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
	"github.com/sendgrid/sendgrid-go"
	"github.com/sendgrid/sendgrid-go/helpers/mail"
	"github.com/stripe/stripe-go/v71"
	"github.com/stripe/stripe-go/v71/checkout/session"
	"github.com/stripe/stripe-go/v71/paymentintent"
	"github.com/zeroshade/tmsapi/internal"
	"github.com/zeroshade/tmsapi/types"
)

func AddStripeRoutes(router *gin.RouterGroup, acctHandler gin.HandlerFunc, db *gorm.DB) {
	router.GET("/stripe/:stripe_session", acctHandler, GetSession(db))
	router.POST("/stripe", acctHandler, CreateSession(db))
}

type createCheckoutSessionResponse struct {
	SessionID string `json:"id"`
}

type Money struct {
	CurrencyCode string  `json:"currency_code"`
	Value        float32 `json:"value,string"`
}

type Item struct {
	Name       string `json:"name"`
	UnitAmount Money  `json:"unit_amount"`
	Quantity   int    `json:"quantity,string"`
	Sku        string `json:"sku"`
	Desc       string `json:"description"`
}

func init() {
	stripe.Key = os.Getenv("STRIPE_KEY")
}

func GetSession(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {

		params := &stripe.CheckoutSessionParams{}
		params.AddExpand("payment_intent.charges")
		params.AddExpand("payment_intent.payment_method")
		params.AddExpand("line_items")
		params.SetStripeAccount(c.GetString("stripe_acct"))
		session, err := session.Get(c.Param("stripe_session"), params)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, session)
	}
}

func CreateSession(db *gorm.DB) gin.HandlerFunc {
	// env := internal.SANDBOX
	// if strings.ToLower(os.Getenv("STRIPE_ENV")) == "live" {
	// 	env = internal.LIVE
	// }

	return func(c *gin.Context) {
		var cart []Item
		if err := c.ShouldBindJSON(&cart); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		params := &stripe.CheckoutSessionParams{
			PaymentMethodTypes: stripe.StringSlice([]string{"card"}),
			Mode:               stripe.String(string(stripe.CheckoutSessionModePayment)),
			SuccessURL:         stripe.String(c.Request.Header.Get("x-calendar-origin") + "?status=success&stripe_session_id={CHECKOUT_SESSION_ID}"),
			CancelURL:          stripe.String(c.Request.Header.Get("x-calendar-origin") + "?status=cancelled&stripe_session_id={CHECKOUT_SESSION_ID}"),
			LineItems:          []*stripe.CheckoutSessionLineItemParams{},
		}

		total := int64(0)
		for _, item := range cart {
			unit := int64(item.UnitAmount.Value * 100)
			quant := int64(item.Quantity)
			total += (unit * quant)
			params.LineItems = append(params.LineItems, &stripe.CheckoutSessionLineItemParams{
				PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
					Currency: stripe.String(string(stripe.CurrencyUSD)),
					ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
						Name: &item.Name,
						Metadata: map[string]string{
							"sku": item.Sku,
						},
					},
					UnitAmount: &unit,
				},
				Quantity: &quant,
			})
		}

		fee := (total / 5000) * 300
		if fee > 0 {
			params.LineItems = append(params.LineItems, &stripe.CheckoutSessionLineItemParams{
				Quantity: stripe.Int64(1),
				PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
					Currency: stripe.String(string(stripe.CurrencyUSD)),
					ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
						Name: stripe.String("Fees"),
					},
					UnitAmount: stripe.Int64(fee),
				},
			})
		}

		params.PaymentIntentData = &stripe.CheckoutSessionPaymentIntentDataParams{
			ApplicationFeeAmount: stripe.Int64(int64(float64(total) * 0.02)),
			Description:          stripe.String("Ticket Purchase"),
		}

		params.SetStripeAccount(c.GetString("stripe_acct"))

		session, err := session.New(params)
		if err != nil {
			c.JSON(http.StatusFailedDependency, gin.H{"error": err.Error()})
			return
		}

		data := createCheckoutSessionResponse{SessionID: session.ID}
		c.JSON(http.StatusOK, data)
	}
}

type PaymentIntent struct {
	ID        string    `json:"id" gorm:"primary_key"`
	Acct      string    `json:"-" gorm:"primary_key"`
	CreatedAt time.Time `json:"createdAt"`
	Amount    string    `json:"amount" gorm:"type:money"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
}

type notifyItem struct {
	Name        string
	Description string
	Quantity    int
}

func sendNotifyEmail(apiKey string, conf *types.MerchantConfig, payment *stripe.PaymentIntent, itemList []notifyItem) error {
	details := payment.Charges.Data[0].BillingDetails

	log.Println("Send Notify Mail:", payment.ID, conf.EmailFrom)
	const tmpl = `
	Tickets Purchased By: {{ .Payer }} <a href='mailto:{{ .PayerEmail }}'>{{ .PayerEmail }}</a>
	<br /><br />
	<ul>
	{{ range .Items -}}
	<li>{{ .Quantity }} {{ .Name }} {{ .Description }}</li>
	</ul>
	{{- end }}`

	t := template.Must(template.New("notify").Parse(tmpl))

	from := mail.NewEmail("Do Not Reply", "donotreply@websbyjoe.org")
	to := mail.NewEmail(conf.EmailName, conf.EmailFrom)
	subject := "Tickets Purchased"
	var tpl bytes.Buffer

	if err := t.Execute(&tpl, gin.H{
		"Payer":      details.Name,
		"PayerEmail": details.Email,
		"Items":      itemList}); err != nil {
		return err
	}

	content := mail.NewContent("text/html", tpl.String())
	log.Println("Send Email:", from, subject, to, content)
	m := mail.NewV3MailInit(from, subject, to, content)
	request := sendgrid.GetRequest(apiKey, "/v3/mail/send", "https://api.sendgrid.com")
	request.Method = "POST"
	request.Body = mail.GetRequestBody(m)
	_, err := sendgrid.API(request)
	if err != nil {
		return err
	}
	return nil
}

func sendCustomerEmail(apiKey, host string, conf *types.MerchantConfig, payment *stripe.PaymentIntent) error {
	details := payment.Charges.Data[0].BillingDetails

	const tmpl = `
	<br /><br />
	Your receipt can be accessed <a href='{{ .Receipt }}'>here</a>.
	<br/>
	If clicking on that doesn't work, you can copy and paste the following URL into
	your browser to access your receipt: {{ .Receipt }}.
	<br /><br/>
	You can download your boarding passes here: <a href='https://{{.Host}}/info/{{.MerchantID}}/passes/{{.PaymentID}}'>Click Here</a>
	<br/>`

	from := mail.NewEmail(conf.EmailName, conf.EmailFrom)
	subject := "Tickets Purchased"
	to := mail.NewEmail(details.Name, details.Email)

	t := template.Must(template.New("notify").Parse(tmpl))
	var tpl bytes.Buffer
	if err := t.Execute(&tpl, gin.H{
		"Receipt": payment.Charges.Data[0].ReceiptURL,
		"Host":    host, "MerchantID": conf.ID, "PaymentID": payment.ID}); err != nil {
		return err
	}

	content := mail.NewContent("text/html", conf.EmailContent+tpl.String())
	log.Println("Send Email:", from, subject, to, content)
	m := mail.NewV3MailInit(from, subject, to, content)
	request := sendgrid.GetRequest(apiKey, "/v3/mail/send", "https://api.sendgrid.com")
	request.Method = "POST"
	request.Body = mail.GetRequestBody(m)
	_, err := sendgrid.API(request)
	if err != nil {
		return err
	}
	return nil
}

type LineItem struct {
	ID        string `json:"id" gorm:"primary_key"`
	PaymentID string `json:"paymentId" gorm:"primary_key"`
	Acct      string `json:"-"`
	Quantity  int    `json:"quantity"`
	Sku       string `json:"sku"`
	Name      string `json:"name"`
	UnitPrice string `json:"unitPrice" gorm:"type:money"`
	Amount    string `json:"total" gorm:"type:money"`
}

func StripeWebhook(db *gorm.DB) gin.HandlerFunc {
	apiKey := os.Getenv("SENDGRID_API_KEY")

	return func(c *gin.Context) {
		event := stripe.Event{}
		if err := c.BindJSON(&event); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		fmt.Println(event.Type)

		var conf types.MerchantConfig
		db.Find(&conf, "stripe_key = ?", event.Account)

		switch event.Type {
		case "payment_intent.succeeded":
			var paymentIntent stripe.PaymentIntent
			if err := json.Unmarshal(event.Data.Raw, &paymentIntent); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}

			details := paymentIntent.Charges.Data[0].BillingDetails

			db.Save(&PaymentIntent{
				ID:        paymentIntent.ID,
				Acct:      event.Account,
				CreatedAt: time.Unix(paymentIntent.Created, 0),
				Amount:    fmt.Sprintf("%0.2f", float64(paymentIntent.Amount)/100.0),
				Email:     details.Email,
				Name:      details.Name,
				Status:    string(paymentIntent.Status),
			})

			err := sendCustomerEmail(apiKey, c.Request.Host, &conf, &paymentIntent)
			if err != nil {
				c.JSON(http.StatusFailedDependency, gin.H{"err": err.Error()})
				return
			}
			c.Status(http.StatusOK)

		case "checkout.session.completed":
			var sess stripe.CheckoutSession
			if err := json.Unmarshal(event.Data.Raw, &sess); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}

			paymentParams := &stripe.PaymentIntentParams{}
			paymentParams.AddExpand("charges")
			paymentParams.AddExpand("payment_method")
			paymentParams.SetStripeAccount(event.Account)
			pm, err := paymentintent.Get(sess.PaymentIntent.ID, paymentParams)
			if err != nil {
				log.Println(err)
			}

			itemList := make([]notifyItem, 0)

			params := &stripe.CheckoutSessionListLineItemsParams{}
			params.AddExpand("data.price")
			params.AddExpand("data.price.product")
			params.SetStripeAccount(event.Account)
			i := session.ListLineItems(sess.ID, params)
			for i.Next() {
				li := i.LineItem()

				itemList = append(itemList, notifyItem{
					Name:     li.Price.Product.Name,
					Quantity: int(li.Quantity),
				})

				db.Save(&LineItem{
					ID:        li.ID,
					PaymentID: sess.PaymentIntent.ID,
					Acct:      event.Account,
					Quantity:  int(li.Quantity),
					Name:      li.Price.Product.Name,
					Sku:       li.Price.Product.Metadata["sku"],
					Amount:    fmt.Sprintf("%0.2f", float64(li.AmountTotal)/100.0),
					UnitPrice: fmt.Sprintf("%0.2f", float64(li.Price.UnitAmount)/100.0),
				})
			}

			if err := sendNotifyEmail(apiKey, &conf, pm, itemList); err != nil {
				c.JSON(http.StatusFailedDependency, gin.H{"error": err.Error()})
				return
			}

			if conf.SendSMS {
				t := internal.NewTwilio(conf.TwilioAcctSID, conf.TwilioAcctToken, conf.TwilioFromNumber)
				t.Send(conf.NotifyNumber, "Tickets Purchased by "+pm.Charges.Data[0].BillingDetails.Name)
			}

		case "charge.refunded":
			var charge stripe.Charge
			if err := json.Unmarshal(event.Data.Raw, &charge); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}

			db.Model(&PaymentIntent{}).Where("id = ?", charge.PaymentIntent.ID).UpdateColumn("status", "refunded")
		}

		c.Status(http.StatusOK)
	}
}
