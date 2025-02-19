package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ente-io/museum/pkg/controller/commonbilling"
	"net/http"
	"strconv"

	"github.com/ente-io/museum/pkg/controller/discord"
	"github.com/ente-io/museum/pkg/controller/offer"
	"github.com/ente-io/museum/pkg/repo/storagebonus"

	"github.com/ente-io/museum/ente"
	emailCtrl "github.com/ente-io/museum/pkg/controller/email"
	"github.com/ente-io/museum/pkg/repo"
	"github.com/ente-io/museum/pkg/utils/billing"
	"github.com/ente-io/museum/pkg/utils/email"
	"github.com/ente-io/stacktrace"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/stripe/stripe-go/v72"
	"github.com/stripe/stripe-go/v72/client"
	"github.com/stripe/stripe-go/v72/invoice"
	"github.com/stripe/stripe-go/v72/webhook"
	"golang.org/x/text/currency"
)

// StripeController provides abstractions for handling billing on Stripe
type StripeController struct {
	StripeClients          ente.StripeClientPerAccount
	BillingPlansPerAccount ente.BillingPlansPerAccount
	BillingRepo            *repo.BillingRepository
	FileRepo               *repo.FileRepository
	UserRepo               *repo.UserRepository
	StorageBonusRepo       *storagebonus.Repository
	DiscordController      *discord.DiscordController
	EmailNotificationCtrl  *emailCtrl.EmailNotificationController
	OfferController        *offer.OfferController
	CommonBillCtrl         *commonbilling.Controller
}

// A flag we set on Stripe subscriptions to indicate that we should skip on
// sending out the email when the subscription has been cancelled.
//
// This is needed e.g. if this cancellation was as part of a user initiated
// account deletion.
const SkipMailKey = "skip_mail"

// Return a new instance of StripeController
func NewStripeController(plans ente.BillingPlansPerAccount, stripeClients ente.StripeClientPerAccount, billingRepo *repo.BillingRepository, fileRepo *repo.FileRepository, userRepo *repo.UserRepository, storageBonusRepo *storagebonus.Repository, discordController *discord.DiscordController, emailNotificationController *emailCtrl.EmailNotificationController, offerController *offer.OfferController, commonBillCtrl *commonbilling.Controller) *StripeController {
	return &StripeController{
		StripeClients:          stripeClients,
		BillingRepo:            billingRepo,
		FileRepo:               fileRepo,
		UserRepo:               userRepo,
		BillingPlansPerAccount: plans,
		StorageBonusRepo:       storageBonusRepo,
		DiscordController:      discordController,
		EmailNotificationCtrl:  emailNotificationController,
		OfferController:        offerController,
		CommonBillCtrl:         commonBillCtrl,
	}
}

// GetCheckoutSession handles the creation of stripe checkout session for subscription purchase
func (c *StripeController) GetCheckoutSession(productID string, userID int64, redirectRootURL string) (string, error) {
	if productID == "" {
		return "", stacktrace.Propagate(ente.ErrBadRequest, "")
	}
	subscription, err := c.BillingRepo.GetUserSubscription(userID)
	if err != nil {
		// error sql.ErrNoRows not possible as user must at least have a free subscription
		return "", stacktrace.Propagate(err, "")
	}
	hasActivePaidSubscription := billing.IsActivePaidPlan(subscription)
	hasStripeSubscription := subscription.PaymentProvider == ente.Stripe
	if hasActivePaidSubscription {
		if hasStripeSubscription {
			return "", stacktrace.Propagate(ente.ErrBadRequest, "")
		} else if !subscription.Attributes.IsCancelled {
			return "", stacktrace.Propagate(ente.ErrBadRequest, "")
		}
	}
	if subscription.PaymentProvider == ente.Stripe && !subscription.Attributes.IsCancelled {
		// user had bought a stripe subscription earlier,
		err := c.cancelExistingStripeSubscription(subscription, userID)
		if err != nil {
			return "", stacktrace.Propagate(err, "")
		}
	}
	stripeSuccessURL := redirectRootURL + viper.GetString("stripe.path.success")
	stripeCancelURL := redirectRootURL + viper.GetString("stripe.path.cancel")
	allowPromotionCodes := true
	params := &stripe.CheckoutSessionParams{
		ClientReferenceID: stripe.String(strconv.FormatInt(userID, 10)),
		SuccessURL:        stripe.String(stripeSuccessURL),
		CancelURL:         stripe.String(stripeCancelURL),
		Mode:              stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Price:    stripe.String(productID),
				Quantity: stripe.Int64(1),
			},
		},
		AllowPromotionCodes: &allowPromotionCodes,
	}
	var stripeClient *client.API
	if subscription.PaymentProvider == ente.Stripe {
		stripeClient = c.StripeClients[subscription.Attributes.StripeAccountCountry]
		// attach the subscription to existing customerID
		params.Customer = stripe.String(subscription.Attributes.CustomerID)
	} else {
		stripeClient = c.StripeClients[ente.DefaultStripeAccountCountry]
		user, err := c.UserRepo.Get(userID)
		if err != nil {
			return "", stacktrace.Propagate(err, "")
		}
		// attach user's emailID to the checkout session and subsequent subscription bought
		params.CustomerEmail = stripe.String(user.Email)
	}

	s, err := stripeClient.CheckoutSessions.New(params)
	if err != nil {
		return "", stacktrace.Propagate(err, "")
	}
	return s.ID, nil
}

// GetVerifiedSubscription verifies and returns the verified subscription
func (c *StripeController) GetVerifiedSubscription(userID int64, sessionID string) (ente.Subscription, error) {
	var stripeSubscription stripe.Subscription
	var err error
	if sessionID != "" {
		log.Info("Received session ID: " + sessionID)
		// Get verified subscription request was received from success redirect page
		stripeSubscription, err = c.getStripeSubscriptionFromSession(userID, sessionID)
	} else {
		log.Info("Did not receive a session ID")
		// Get verified subscription request for a subscription update
		stripeSubscription, err = c.getUserStripeSubscription(userID)
	}
	if err != nil {
		return ente.Subscription{}, stacktrace.Propagate(err, "")
	}
	log.Info("Received stripe subscription with ID: " + stripeSubscription.ID)
	subscription, err := c.getEnteSubscriptionFromStripeSubscription(userID, stripeSubscription)
	if err != nil {
		return ente.Subscription{}, stacktrace.Propagate(err, "")
	}
	log.Info("Returning ente subscription with ID: " + strconv.FormatInt(subscription.ID, 10))
	return subscription, nil
}

func (c *StripeController) HandleUSNotification(payload []byte, header string) error {
	event, err := webhook.ConstructEvent(payload, header, viper.GetString("stripe.us.webhook-secret"))
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	return c.handleWebhookEvent(event)
}

func (c *StripeController) HandleINNotification(payload []byte, header string) error {
	event, err := webhook.ConstructEvent(payload, header, viper.GetString("stripe.in.webhook-secret"))
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	return c.handleWebhookEvent(event)
}

func (c *StripeController) handleWebhookEvent(event stripe.Event) error {
	// The event body would already have been logged by the upper layers by the
	// time we get here, so we can only handle the events that we care about. In
	// case we receive an unexpected event, we do log an error though.
	handler := c.findHandlerForEvent(event)
	if handler == nil {
		log.Error("Received an unexpected webhook from stripe:", event.Type)
		return nil
	}
	eventLog, err := handler(event)
	if err != nil {
		return stacktrace.Propagate(err, "")
	}
	if eventLog.UserID == 0 {
		// Do not try to log if we do not have an associated user. This can
		// happen, e.g. with out of order webhooks.
		// Or in case of offer application, where events are logged by the Storage Bonus Repo
		//
		// See: Ignore webhooks received before user has been created
		return nil
	}
	err = c.BillingRepo.LogStripePush(eventLog)
	return stacktrace.Propagate(err, "")
}

func (c *StripeController) findHandlerForEvent(event stripe.Event) func(event stripe.Event) (ente.StripeEventLog, error) {
	switch event.Type {
	case "checkout.session.completed":
		return c.handleCheckoutSessionCompleted
	case "customer.subscription.deleted":
		return c.handleCustomerSubscriptionDeleted
	case "customer.subscription.updated":
		return c.handleCustomerSubscriptionUpdated
	case "invoice.paid":
		return c.handleInvoicePaid
	default:
		return nil
	}
}

// Payment is successful and the subscription is created.
// You should provision the subscription.
func (c *StripeController) handleCheckoutSessionCompleted(event stripe.Event) (ente.StripeEventLog, error) {
	var session stripe.CheckoutSession
	json.Unmarshal(event.Data.Raw, &session)
	if session.ClientReferenceID != "" { // via payments.ente.io, where we inserted the userID
		userID, _ := strconv.ParseInt(session.ClientReferenceID, 10, 64)
		newSubscription, err := c.GetVerifiedSubscription(userID, session.ID)
		if err != nil {
			return ente.StripeEventLog{}, stacktrace.Propagate(err, "")
		}
		stripeSubscription, err := c.getStripeSubscriptionFromSession(userID, session.ID)
		if err != nil {
			return ente.StripeEventLog{}, stacktrace.Propagate(err, "")
		}
		currentSubscription, err := c.BillingRepo.GetUserSubscription(userID)
		if err != nil {
			return ente.StripeEventLog{}, stacktrace.Propagate(err, "")
		}
		if currentSubscription.ExpiryTime >= newSubscription.ExpiryTime &&
			currentSubscription.ProductID != ente.FreePlanProductID {
			log.Warn("Webhook is reporting an outdated purchase that was already verified stripeSubscription:", stripeSubscription.ID)
			return ente.StripeEventLog{UserID: userID, StripeSubscription: stripeSubscription, Event: event}, nil
		}
		err = c.BillingRepo.ReplaceSubscription(
			currentSubscription.ID,
			newSubscription,
		)
		isUpgradingFromFreePlan := currentSubscription.ProductID == ente.FreePlanProductID
		if isUpgradingFromFreePlan {
			go func() {
				cur := currency.MustParseISO(string(session.Currency))
				amount := fmt.Sprintf("%v%v", currency.Symbol(cur), float64(session.AmountTotal)/float64(100))
				c.DiscordController.NotifyNewSub(userID, "stripe", amount)
			}()
			go func() {
				c.EmailNotificationCtrl.OnAccountUpgrade(userID)
			}()
		}
		if err != nil {
			return ente.StripeEventLog{}, stacktrace.Propagate(err, "")
		}
		return ente.StripeEventLog{UserID: userID, StripeSubscription: stripeSubscription, Event: event}, nil
	} else {
		priceID, err := c.getPriceIDFromSession(session.ID)
		if err != nil {
			return ente.StripeEventLog{}, stacktrace.Propagate(err, "")
		}
		email := session.CustomerDetails.Email
		err = c.OfferController.ApplyOffer(email, priceID)
		if err != nil {
			return ente.StripeEventLog{}, stacktrace.Propagate(err, "")
		}
	}
	return ente.StripeEventLog{}, nil
}

// Occurs whenever a customer's subscription ends.
func (c *StripeController) handleCustomerSubscriptionDeleted(event stripe.Event) (ente.StripeEventLog, error) {
	var stripeSubscription stripe.Subscription
	json.Unmarshal(event.Data.Raw, &stripeSubscription)
	currentSubscription, err := c.BillingRepo.GetSubscriptionForTransaction(stripeSubscription.ID, ente.Stripe)
	if err != nil {
		// Ignore webhooks received before user has been created
		//
		// This would happen when we get webhook events out of order, e.g. we
		// get a "customer.subscription.updated" before
		// "checkout.session.completed", and the customer has not yet been
		// created in our database.
		if errors.Is(err, sql.ErrNoRows) {
			log.Warn("Webhook is reporting an event for un-verified subscription stripeSubscriptionID:", stripeSubscription.ID)
			return ente.StripeEventLog{}, nil
		}
		return ente.StripeEventLog{}, stacktrace.Propagate(err, "")
	}
	userID := currentSubscription.UserID
	user, err := c.UserRepo.Get(userID)
	if err != nil {
		if errors.Is(err, ente.ErrUserDeleted) {
			// no-op user has already been deleted
			return ente.StripeEventLog{UserID: userID, StripeSubscription: stripeSubscription, Event: event}, nil
		}
		return ente.StripeEventLog{}, stacktrace.Propagate(err, "")
	}

	skipMail := stripeSubscription.Metadata[SkipMailKey]
	// Send a cancellation notification email for folks who are either on
	// individual plan or admin of a family plan.
	if skipMail != "true" &&
		(user.FamilyAdminID == nil || *user.FamilyAdminID == userID) {
		storage, surpErr := c.StorageBonusRepo.GetPaidAddonSurplusStorage(context.Background(), userID)
		if surpErr != nil {
			return ente.StripeEventLog{}, stacktrace.Propagate(surpErr, "")
		}
		if storage == nil || *storage <= 0 {
			err = email.SendTemplatedEmail([]string{user.Email}, "ente", "support@ente.io",
				ente.SubscriptionEndedEmailSubject, ente.SubscriptionEndedEmailTemplate,
				map[string]interface{}{}, nil)
			if err != nil {
				return ente.StripeEventLog{}, stacktrace.Propagate(err, "")
			}
		} else {
			log.WithField("storage", storage).Info("User has surplus storage, not sending email")
		}
	}
	// TODO: Add cron to delete files of users with expired subscriptions
	return ente.StripeEventLog{UserID: userID, StripeSubscription: stripeSubscription, Event: event}, nil
}

// Occurs whenever a subscription changes (e.g., switching from one plan to
// another, or changing the status from trial to active).
func (c *StripeController) handleCustomerSubscriptionUpdated(event stripe.Event) (ente.StripeEventLog, error) {
	var stripeSubscription stripe.Subscription
	json.Unmarshal(event.Data.Raw, &stripeSubscription)
	currentSubscription, err := c.BillingRepo.GetSubscriptionForTransaction(stripeSubscription.ID, ente.Stripe)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// See: Ignore webhooks received before user has been created
			log.Warn("Webhook is reporting an event for un-verified subscription stripeSubscriptionID:", stripeSubscription.ID)
			return ente.StripeEventLog{}, nil
		}
		return ente.StripeEventLog{}, stacktrace.Propagate(err, "")
	}

	userID := currentSubscription.UserID
	switch stripeSubscription.Status {
	case stripe.SubscriptionStatusPastDue:
		user, err := c.UserRepo.Get(userID)
		if err != nil {
			return ente.StripeEventLog{}, stacktrace.Propagate(err, "")
		}
		err = email.SendTemplatedEmail([]string{user.Email}, "ente", "support@ente.io",
			ente.AccountOnHoldEmailSubject, ente.OnHoldTemplate, map[string]interface{}{
				"PaymentProvider": "Stripe",
			}, nil)
		if err != nil {
			return ente.StripeEventLog{}, stacktrace.Propagate(err, "")
		}
	case stripe.SubscriptionStatusActive:
		newSubscription, err := c.getEnteSubscriptionFromStripeSubscription(userID, stripeSubscription)
		if err != nil {
			return ente.StripeEventLog{}, stacktrace.Propagate(err, "")
		}
		if currentSubscription.ProductID == newSubscription.ProductID {
			// Webhook is reporting an outdated update that was already verified
			// no-op
			log.Warn("Webhook is reporting an outdated purchase that was already verified stripeSubscriptionID:", stripeSubscription.ID)
			return ente.StripeEventLog{UserID: userID, StripeSubscription: stripeSubscription, Event: event}, nil
		}
		if newSubscription.ProductID != currentSubscription.ProductID {
			c.BillingRepo.ReplaceSubscription(currentSubscription.ID, newSubscription)
		}
	}
	return ente.StripeEventLog{UserID: userID, StripeSubscription: stripeSubscription, Event: event}, nil
}

// Continue to provision the subscription as payments continue to be made.
func (c *StripeController) handleInvoicePaid(event stripe.Event) (ente.StripeEventLog, error) {
	var invoice stripe.Invoice
	json.Unmarshal(event.Data.Raw, &invoice)
	stripeSubscriptionID := invoice.Subscription.ID
	currentSubscription, err := c.BillingRepo.GetSubscriptionForTransaction(stripeSubscriptionID, ente.Stripe)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// See: Ignore webhooks received before user has been created
			log.Warn("Webhook is reporting an event for un-verified subscription stripeSubscriptionID:", stripeSubscriptionID)
			return ente.StripeEventLog{}, nil
		}
		return ente.StripeEventLog{}, stacktrace.Propagate(err, "")
	}

	userID := currentSubscription.UserID
	client := c.StripeClients[currentSubscription.Attributes.StripeAccountCountry]

	stripeSubscription, err := client.Subscriptions.Get(stripeSubscriptionID, nil)
	if err != nil {
		return ente.StripeEventLog{}, stacktrace.Propagate(err, "")
	}

	newExpiryTime := stripeSubscription.CurrentPeriodEnd * 1000 * 1000
	if currentSubscription.ExpiryTime == newExpiryTime {
		//outdated invoice
		log.Warn("Webhook is reporting an outdated purchase that was already verified stripeSubscriptionID:", stripeSubscription.ID)
		return ente.StripeEventLog{UserID: userID, StripeSubscription: *stripeSubscription, Event: event}, nil
	}
	err = c.BillingRepo.UpdateSubscriptionExpiryTime(
		currentSubscription.ID, newExpiryTime)
	if err != nil {
		return ente.StripeEventLog{}, stacktrace.Propagate(err, "")
	}
	return ente.StripeEventLog{UserID: userID, StripeSubscription: *stripeSubscription, Event: event}, nil
}

func (c *StripeController) UpdateSubscription(stripeID string, userID int64) (ente.SubscriptionUpdateResponse, error) {
	subscription, err := c.BillingRepo.GetUserSubscription(userID)
	if err != nil {
		return ente.SubscriptionUpdateResponse{}, stacktrace.Propagate(err, "")
	}
	newPlan, newStripeAccountCountry, err := c.getPlanAndAccount(stripeID)
	if err != nil {
		return ente.SubscriptionUpdateResponse{}, stacktrace.Propagate(err, "")
	}
	if subscription.PaymentProvider != ente.Stripe || subscription.ProductID == stripeID || subscription.Attributes.StripeAccountCountry != newStripeAccountCountry {
		return ente.SubscriptionUpdateResponse{}, stacktrace.Propagate(ente.ErrBadRequest, "")
	}
	if newPlan.Storage < subscription.Storage { // Downgrade
		canDowngrade, canDowngradeErr := c.CommonBillCtrl.CanDowngradeToGivenStorage(newPlan.Storage, userID)
		if canDowngradeErr != nil {
			return ente.SubscriptionUpdateResponse{}, stacktrace.Propagate(canDowngradeErr, "")
		}
		if !canDowngrade {
			return ente.SubscriptionUpdateResponse{}, stacktrace.Propagate(ente.ErrCannotDowngrade, "")
		}
		log.Info("Usage is good")

	}
	client := c.StripeClients[subscription.Attributes.StripeAccountCountry]
	stripeSubscription, err := client.Subscriptions.Get(subscription.OriginalTransactionID, nil)
	if err != nil {
		return ente.SubscriptionUpdateResponse{}, stacktrace.Propagate(err, "")
	}
	params := stripe.SubscriptionParams{
		ProrationBehavior: stripe.String(string(stripe.SubscriptionProrationBehaviorAlwaysInvoice)),
		Items: []*stripe.SubscriptionItemsParams{
			{
				ID:    stripe.String(stripeSubscription.Items.Data[0].ID),
				Price: stripe.String(stripeID),
			},
		},
		PaymentBehavior: stripe.String(string(stripe.SubscriptionPaymentBehaviorPendingIfIncomplete)),
	}
	params.AddExpand("latest_invoice.payment_intent")
	newStripeSubscription, err := client.Subscriptions.Update(subscription.OriginalTransactionID, &params)
	if err != nil {
		stripeError := err.(*stripe.Error)
		switch stripeError.Type {
		case stripe.ErrorTypeCard:
			return ente.SubscriptionUpdateResponse{Status: "requires_payment_method"}, nil
		default:
			return ente.SubscriptionUpdateResponse{}, stacktrace.Propagate(err, "")
		}
	}
	if newStripeSubscription.PendingUpdate != nil {
		switch newStripeSubscription.LatestInvoice.PaymentIntent.Status {
		case stripe.PaymentIntentStatusRequiresAction:
			return ente.SubscriptionUpdateResponse{Status: "requires_action", ClientSecret: newStripeSubscription.LatestInvoice.PaymentIntent.ClientSecret}, nil
		case stripe.PaymentIntentStatusRequiresPaymentMethod:
			inv := newStripeSubscription.LatestInvoice
			invoice.VoidInvoice(inv.ID, nil)
			return ente.SubscriptionUpdateResponse{Status: "requires_payment_method"}, nil
		}
		return ente.SubscriptionUpdateResponse{}, stacktrace.Propagate(ente.ErrBadRequest, "")
	}
	return ente.SubscriptionUpdateResponse{Status: "success"}, nil

}

func (c *StripeController) UpdateSubscriptionCancellationStatus(userID int64, status bool) (ente.Subscription, error) {
	subscription, err := c.BillingRepo.GetUserSubscription(userID)
	if err != nil {
		// error sql.ErrNoRows not possible as user must at least have a free subscription
		return ente.Subscription{}, stacktrace.Propagate(err, "")
	}
	if subscription.PaymentProvider != ente.Stripe {
		return ente.Subscription{}, stacktrace.Propagate(ente.ErrBadRequest, "")
	}

	if subscription.Attributes.IsCancelled == status {
		// no-op
		return subscription, nil
	}

	client := c.StripeClients[subscription.Attributes.StripeAccountCountry]
	params := &stripe.SubscriptionParams{
		CancelAtPeriodEnd: stripe.Bool(status),
	}
	_, err = client.Subscriptions.Update(subscription.OriginalTransactionID, params)
	if err != nil {
		return ente.Subscription{}, stacktrace.Propagate(err, "")
	}
	err = c.BillingRepo.UpdateSubscriptionCancellationStatus(userID, status)
	if err != nil {
		return ente.Subscription{}, stacktrace.Propagate(err, "")
	}
	subscription.Attributes.IsCancelled = status
	return subscription, nil
}

func (c *StripeController) GetStripeCustomerPortal(userID int64, redirectRootURL string) (string, error) {
	subscription, err := c.BillingRepo.GetUserSubscription(userID)
	if err != nil {
		return "", stacktrace.Propagate(err, "")
	}
	if subscription.PaymentProvider != ente.Stripe {
		return "", stacktrace.Propagate(ente.ErrBadRequest, "")
	}
	client := c.StripeClients[subscription.Attributes.StripeAccountCountry]

	params := &stripe.BillingPortalSessionParams{
		Customer:  stripe.String(subscription.Attributes.CustomerID),
		ReturnURL: stripe.String(redirectRootURL),
	}
	ps, err := client.BillingPortalSessions.New(params)
	if err != nil {
		return "", stacktrace.Propagate(err, "")
	}
	return ps.URL, nil
}

func (c *StripeController) getStripeSubscriptionFromSession(userID int64, checkoutSessionID string) (stripe.Subscription, error) {
	subscription, err := c.BillingRepo.GetUserSubscription(userID)
	if err != nil {
		return stripe.Subscription{}, stacktrace.Propagate(err, "")
	}
	var stripeClient *client.API
	if subscription.PaymentProvider == ente.Stripe {
		stripeClient = c.StripeClients[subscription.Attributes.StripeAccountCountry]
	} else {
		stripeClient = c.StripeClients[ente.DefaultStripeAccountCountry]
	}
	params := &stripe.CheckoutSessionParams{}
	params.AddExpand("subscription")
	checkoutSession, err := stripeClient.CheckoutSessions.Get(checkoutSessionID, params)
	if err != nil {
		return stripe.Subscription{}, stacktrace.Propagate(err, "")
	}
	if (*checkoutSession.Subscription).Status != stripe.SubscriptionStatusActive {
		return stripe.Subscription{}, stacktrace.Propagate(&stripe.InvalidRequestError{}, "")
	}
	return *checkoutSession.Subscription, nil
}

func (c *StripeController) getPriceIDFromSession(sessionID string) (string, error) {
	stripeClient := c.StripeClients[ente.DefaultStripeAccountCountry]
	params := &stripe.CheckoutSessionListLineItemsParams{}
	params.AddExpand("data.price")
	items := stripeClient.CheckoutSessions.ListLineItems(sessionID, params)
	for items.Next() { // Return the first PriceID that has been fetched
		return items.LineItem().Price.ID, nil
	}
	return "", stacktrace.Propagate(ente.ErrNotFound, "")
}

func (c *StripeController) getUserStripeSubscription(userID int64) (stripe.Subscription, error) {
	subscription, err := c.BillingRepo.GetUserSubscription(userID)
	if err != nil {
		return stripe.Subscription{}, stacktrace.Propagate(err, "")
	}
	if subscription.PaymentProvider != ente.Stripe {
		return stripe.Subscription{}, stacktrace.Propagate(ente.ErrCannotSwitchPaymentProvider, "")
	}
	client := c.StripeClients[subscription.Attributes.StripeAccountCountry]
	stripeSubscription, err := client.Subscriptions.Get(subscription.OriginalTransactionID, nil)
	if err != nil {
		return stripe.Subscription{}, stacktrace.Propagate(err, "")
	}
	return *stripeSubscription, nil
}

func (c *StripeController) getPlanAndAccount(stripeID string) (ente.BillingPlan, ente.StripeAccountCountry, error) {
	for stripeAccountCountry, billingPlansCountryWise := range c.BillingPlansPerAccount {
		for _, plans := range billingPlansCountryWise {
			for _, plan := range plans {
				if plan.StripeID == stripeID {
					return plan, stripeAccountCountry, nil
				}
			}
		}
	}
	return ente.BillingPlan{}, "", stacktrace.Propagate(ente.ErrNotFound, "")
}

func (c *StripeController) getEnteSubscriptionFromStripeSubscription(userID int64, stripeSubscription stripe.Subscription) (ente.Subscription, error) {
	productID := stripeSubscription.Items.Data[0].Price.ID
	plan, stripeAccountCountry, err := c.getPlanAndAccount(productID)
	if err != nil {
		return ente.Subscription{}, stacktrace.Propagate(err, "")
	}
	s := ente.Subscription{
		UserID:                userID,
		PaymentProvider:       ente.Stripe,
		ProductID:             productID,
		Storage:               plan.Storage,
		Attributes:            ente.SubscriptionAttributes{CustomerID: stripeSubscription.Customer.ID, IsCancelled: false, StripeAccountCountry: stripeAccountCountry},
		OriginalTransactionID: stripeSubscription.ID,
		ExpiryTime:            stripeSubscription.CurrentPeriodEnd * 1000 * 1000,
	}
	return s, nil
}

func (c *StripeController) UpdateBillingEmail(subscription ente.Subscription, newEmail string) error {
	params := &stripe.CustomerParams{Email: &newEmail}
	client := c.StripeClients[subscription.Attributes.StripeAccountCountry]
	_, err := client.Customers.Update(
		subscription.Attributes.CustomerID,
		params,
	)
	if err != nil {
		return stacktrace.Propagate(err, "failed to update stripe customer emailID")
	}
	return nil
}

func (c *StripeController) CancelSubAndDeleteCustomer(subscription ente.Subscription, logger *log.Entry) error {
	client := c.StripeClients[subscription.Attributes.StripeAccountCountry]
	if !subscription.Attributes.IsCancelled {
		prorateRefund := true
		logger.Info("cancelling sub with prorated refund")
		updateParams := &stripe.SubscriptionParams{}
		updateParams.AddMetadata(SkipMailKey, "true")
		_, err := client.Subscriptions.Update(subscription.OriginalTransactionID, updateParams)
		if err != nil {
			stripeError := err.(*stripe.Error)
			errorMsg := fmt.Sprintf("subscription updation failed during account deletion: %s, %s", stripeError.Msg, stripeError.Code)
			log.Error(errorMsg)
			c.DiscordController.Notify(errorMsg)
			if stripeError.HTTPStatusCode == http.StatusNotFound {
				log.Error("Ignoring error since an active subscription could not be found")
				return nil
			} else if stripeError.HTTPStatusCode == http.StatusBadRequest {
				log.Error("Bad request while trying to delete account")
				return nil
			}
			return stacktrace.Propagate(err, "")
		}
		_, err = client.Subscriptions.Cancel(subscription.OriginalTransactionID, &stripe.SubscriptionCancelParams{
			Prorate: &prorateRefund,
		})
		if err != nil {
			stripeError := err.(*stripe.Error)
			logger.Error(fmt.Sprintf("subscription cancel failed msg= %s for userID=%d"+stripeError.Msg, subscription.UserID))
			// ignore if subscription doesn't exist, already deleted
			if stripeError.HTTPStatusCode != 404 {
				return stacktrace.Propagate(err, "")
			}
		}
		err = c.BillingRepo.UpdateSubscriptionCancellationStatus(subscription.UserID, true)
		if err != nil {
			return stacktrace.Propagate(err, "")
		}
	}
	logger.Info("deleting customer from stripe")
	_, err := client.Customers.Del(
		subscription.Attributes.CustomerID,
		&stripe.CustomerParams{},
	)
	if err != nil {
		stripeError := err.(*stripe.Error)
		switch stripeError.Type {
		case stripe.ErrorTypeInvalidRequest:
			if stripe.ErrorCodeResourceMissing == stripeError.Code {
				return nil
			}
			return stacktrace.Propagate(err, fmt.Sprintf("failed to delete customer %s", subscription.Attributes.CustomerID))
		default:
			return stacktrace.Propagate(err, fmt.Sprintf("failed to delete customer %s", subscription.Attributes.CustomerID))
		}
	}
	return nil
}

// cancel the earlier past_due subscription
// and add skip mail metadata entry to avoid sending account deletion mail while re-subscription
func (c *StripeController) cancelExistingStripeSubscription(subscription ente.Subscription, userID int64) error {
	updateParams := &stripe.SubscriptionParams{}
	updateParams.AddMetadata(SkipMailKey, "true")
	client := c.StripeClients[subscription.Attributes.StripeAccountCountry]
	_, err := client.Subscriptions.Update(subscription.OriginalTransactionID, updateParams)
	if err != nil {
		stripeError := err.(*stripe.Error)
		log.Warn(fmt.Sprintf("subscription updation failed msg= %s for userID=%d", stripeError.Msg, userID))
		// ignore if subscription doesn't exist, already deleted
		if stripeError.HTTPStatusCode != 404 {
			return stacktrace.Propagate(err, "")
		}
	} else {
		_, err = client.Subscriptions.Cancel(subscription.OriginalTransactionID, nil)
		if err != nil {
			stripeError := err.(*stripe.Error)
			log.Warn(fmt.Sprintf("subscription cancel failed msg= %s for userID=%d", stripeError.Msg, userID))
			// ignore if subscription doesn't exist, already deleted
			if stripeError.HTTPStatusCode != 404 {
				return stacktrace.Propagate(err, "")
			}
		}
		err = c.BillingRepo.UpdateSubscriptionCancellationStatus(userID, true)
		if err != nil {
			return stacktrace.Propagate(err, "")
		}
	}
	return nil
}
