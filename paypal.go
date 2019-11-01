package main

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"hash/crc32"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

type amount struct {
	Total    float32 `json:"total,string"`
	Currency string  `json:"currency" gorm:"-"`
}

type link struct {
	Href    string `json:"href"`
	Rel     string `json:"rel"`
	Method  string `json:"method"`
	EncType string `json:"encType"`
}

type cutime struct {
	UpdateTime time.Time `json:"update_time"`
	CreateTime time.Time `json:"create_time"`
}

type PayerInfo struct {
	Email     string `json:"email"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	PayerID   string `json:"payer_id" gorm:"primary_key"`
	Phone     string `json:"phone"`
	Country   string `json:"country_code"`
}

type WebHookEvent struct {
	ID            string    `json:"id"`
	CreateTime    time.Time `json:"create_time"`
	ResourceType  string    `json:"resource_type"`
	EventType     string    `json:"event_type"`
	Summary       string    `json:"summary"`
	Resource      interface{}
	Status        string `json:"status"`
	Transmissions []struct {
		WebhookURL     string `json:"webhook_url"`
		TransmissionID string `json:"transmission_id"`
		Status         string `json:"status"`
	} `json:"transmissions"`
	Links        []link `json:"links"`
	EventVersion string `json:"event_version"`
}

func (w *WebHookEvent) UnmarshalJSON(data []byte) error {
	type Alias WebHookEvent
	aux := &struct {
		*Alias
		RawResource *json.RawMessage `json:"resource"`
	}{
		Alias: (*Alias)(w),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	switch aux.ResourceType {
	case "sale":
		aux.Resource = new(Sale)
	case "payment":
		aux.Resource = new(Payment)
	}

	return json.Unmarshal(*aux.RawResource, aux.Resource)
}

type item struct {
	Name     string  `json:"name"`
	Sku      string  `json:"sku"`
	Price    float32 `json:"price,string"`
	Currency string  `json:"currency"`
	Tax      float32 `json:"tax,string"`
	Qty      uint32  `json:"quantity"`
}

type Transaction struct {
	TransactionID uint   `json:"-" gorm:"primary_key"`
	PaymentID     string `json:"-"`
	Amount        amount `json:"amount" gorm:"embedded"`
	Payee         struct {
		MerchantID string `json:"merchant_id"`
		Email      string `json:"email"`
	} `json:"payee" gorm:"embedded;embedded_prefix:payee_"`
	Desc     string `json:"description"`
	SoftDesc string `json:"soft_descriptor"`
	ItemList struct {
		Items []item `json:"items"`
	} `json:"item_list" gorm:"-"`

	RelatedResources []interface{} `gorm:"-"`
}

func (t *Transaction) UnmarshalJSON(data []byte) error {
	type Alias Transaction
	aux := &struct {
		*Alias
		Related []map[string]*json.RawMessage `json:"related_resources"`
	}{
		Alias: (*Alias)(t),
	}
	err := json.Unmarshal(data, &aux)
	if err != nil {
		return err
	}

	for _, m := range aux.Related {
		for k, v := range m {
			if k == "sale" {
				s := new(Sale)
				if err = json.Unmarshal(*v, s); err != nil {
					return err
				}

				aux.RelatedResources = append(aux.RelatedResources, s)
			}
		}
	}

	return nil
}

type Payment struct {
	cutime
	ID           string        `json:"id" gorm:"primary_key"`
	Links        []link        `json:"links"`
	State        string        `json:"state"`
	Transactions []Transaction `json:"transactions"`
	Intent       string        `json:"intent"`
	Payer        struct {
		PaymentMethod string    `json:"payment_method"`
		Status        string    `json:"status"`
		PayerID       uint      `json:"-"`
		PayerInfo     PayerInfo `json:"payer_info"`
	} `json:"payer" gorm:"embedded;embedded_prefix:payer_"`
	CartID string `json:"cart"`
}

type Sale struct {
	cutime
	ID             string `json:"id" gorm:"primary_key"`
	Amount         amount `json:"amount" gorm:"embedded"`
	PaymentMode    string `json:"payment_mode"`
	TransactionFee struct {
		Value    float32 `json:"value,string" gorm:"column:transaction_fee"`
		Currency string  `json:"currency" gorm:"-"`
	} `json:"transaction_fee" gorm:"embedded"`
	ParentPayment   string `json:"parent_payment"`
	SoftDesc        string `json:"soft_descriptor"`
	ProtectEligible string `json:"protection_eligibility"`
	Links           []link `json:"links"`
	State           string `json:"state"`
	InvoiceNum      string `json:"invoice_number"`
}

// WebhookID is the constant id from PayPal for this webhook
var WebhookID string

func init() {
	WebhookID = os.Getenv("WEBHOOK_ID")
}

// HandlePaypalWebhook returns a handler function that verifies a paypal webhook
// post request and then processes the event message
func HandlePaypalWebhook(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		sig := c.GetHeader("PAYPAL-TRANSMISSION-SIG")
		certurl := c.GetHeader("PAYPAL-CERT-URL")

		transmissionid := c.GetHeader("PAYPAL-TRANSMISSION-ID")
		timestamp := c.GetHeader("PAYPAL-TRANSMISSION-TIME")
		webhookid := WebhookID

		defer c.Request.Body.Close()
		body, err := ioutil.ReadAll(c.Request.Body)
		if err != nil {
			log.Println(err)
			c.Status(http.StatusBadRequest)
			return
		}

		cert, err := GetCert(certurl)
		if err != nil {
			log.Println(err)
			c.Status(http.StatusBadRequest)
			return
		}

		if !VerifySig(cert, transmissionid, timestamp, webhookid, sig, body) {
			log.Println("Didn't Verify")
			c.Status(http.StatusBadRequest)
			return
		}

		var we WebHookEvent
		json.Unmarshal(body, &we)

		switch we.ResourceType {
		case "payment":
			handlePayment(db, &we)
		case "sale":
		}

		c.Status(http.StatusOK)
	}
}

func handlePayment(db *gorm.DB, we *WebHookEvent) {
	res := we.Resource.(*Payment)

	db.Save(res)
}

// GetCert retrieves the PEM certificate using the URL that was provided
func GetCert(certurl string) (*x509.Certificate, error) {
	resp, err := http.Get(certurl)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	block, _ := pem.Decode(body)
	return x509.ParseCertificate(block.Bytes)
}

// VerifySig takes in the certificate and necessary data to validate the signature that
// was provided for this webhook post request
func VerifySig(cert *x509.Certificate, transid, timestamp, webhookid, sig string, body []byte) bool {
	if cert == nil {
		return false
	}

	crc := crc32.ChecksumIEEE(body)
	expectsig := strings.Join([]string{transid, timestamp, webhookid, strconv.Itoa(int(crc))}, "|")

	data, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return false
	}

	return cert.CheckSignature(cert.SignatureAlgorithm, []byte(expectsig), data) == nil
}
