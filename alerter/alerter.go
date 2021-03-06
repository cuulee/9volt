package alerter

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	log "github.com/Sirupsen/logrus"
	"github.com/relistan/go-director"
	gouuid "github.com/satori/go.uuid"

	"github.com/9corp/9volt/config"
	"github.com/9corp/9volt/util"
)

type IAlerter interface {
	Send(*Message, *AlerterConfig) error
	Identify() string
	ValidateConfig(*AlerterConfig) error
}

type AlerterConfig struct {
	Type        string            `json:"type"`
	Description string            `json:"description"`
	Options     map[string]string `json:"options"`
}

type Alerter struct {
	Identifier     string
	MemberID       string
	Config         *config.Config
	Alerters       map[string]IAlerter
	MessageChannel <-chan *Message
	Looper         director.Looper
}

type Message struct {
	Type     string            // "resolve", "warning", "critical"
	Key      []string          // Keys coming from the monitor config for Critical or WarningAlerters
	Title    string            // Short description of the alert
	Text     string            // In-depth description of the alert state
	Source   string            // Origin of the alert
	Count    int               // How many check attempts were made
	Contents map[string]string // Set checker-specific data (ensuring alerters know how to use the data)
	uuid     string            // For private use within the alerter
}

func New(cfg *config.Config, messageChannel <-chan *Message) *Alerter {
	return &Alerter{
		Identifier:     "alerter",
		MemberID:       cfg.MemberID,
		Config:         cfg,
		MessageChannel: messageChannel,
		Looper:         director.NewFreeLooper(director.FOREVER, make(chan error)),
	}
}

func (a *Alerter) Start() error {
	log.Infof("%v: Starting alerter components...", a.Identifier)

	// Instantiate our alerters
	pagerduty := NewPagerduty(a.Config)
	slack := NewSlack(a.Config)
	email := NewEmail(a.Config)

	a.Alerters = map[string]IAlerter{
		pagerduty.Identify(): pagerduty,
		slack.Identify():     slack,
		email.Identify():     email,
	}

	// Launch our alerter message handler
	go a.run()

	return nil
}

func (a *Alerter) run() error {
	a.Looper.Loop(func() error {
		msg := <-a.MessageChannel

		// tag message
		msg.uuid = gouuid.NewV4().String()

		log.Debugf("%v: Received message (%v) from checker '%v' -> '%v'", msg.uuid, a.Identifier, msg.Source, msg.Key)

		go a.handleMessage(msg)

		return nil
	})

	return nil
}

func (a *Alerter) handleMessage(msg *Message) error {
	// validate message contents
	if err := a.validateMessage(msg); err != nil {
		a.Config.EQClient.AddWithErrorLog("error",
			fmt.Sprintf("%v: Unable to validate message %v: %v", a.Identifier, msg.uuid, err.Error()))
		return err
	}

	errorList := make([]string, 0)

	// fetch alert configuration for each individual key, send alert for each.
	// keep track of any encountered errors; report result in the end
	for _, alerterKey := range msg.Key {
		alerterConfig, err := a.loadAlerterConfig(alerterKey, msg)
		if err != nil {
			errorMessage := fmt.Sprintf("Unable to load alerter key for %v: %v", msg.uuid, err.Error())
			errorList = append(errorList, errorMessage)
			log.Errorf("%v: %v", a.Identifier, errorMessage)
			continue
		}

		// validate the alerter config
		if err := a.Alerters[alerterConfig.Type].ValidateConfig(alerterConfig); err != nil {
			errorMessage := fmt.Sprintf("Unable to validate alerter config for %v: %v", msg.uuid, err.Error())
			errorList = append(errorList, errorMessage)
			log.Errorf("%v: %v", a.Identifier, errorMessage)
			continue
		}

		// send the actual alert
		log.Debugf("%v: Sending %v to alerter %v!", a.Identifier, msg.uuid, alerterConfig.Type)
		if err := a.Alerters[alerterConfig.Type].Send(msg, alerterConfig); err != nil {
			errorMessage := fmt.Sprintf("Unable to complete message send for %v: %v", msg.uuid, err.Error())
			errorList = append(errorList, errorMessage)
			log.Errorf("%v: %v", a.Identifier, errorMessage)
			continue
		}
	}

	if len(errorList) != 0 {
		a.Config.EQClient.AddWithErrorLog("error",
			fmt.Sprintf("%v: Ran into %v errors during alert send for %v (alerters: %v); error list: %v",
				a.Identifier, len(errorList), msg.Source, msg.Key, strings.Join(errorList, "; ")))
	} else {
		log.Debugf("%v: Successfully sent %v alert messages for %v (alerters: %v)",
			a.Identifier, len(msg.Key), msg.uuid, msg.Key)
	}

	return nil
}

// Fetch an alert config for a given alert key; ensure we can unmarshal it
func (a *Alerter) loadAlerterConfig(alerterKey string, msg *Message) (*AlerterConfig, error) {
	jsonAlerterConfig, err := a.Config.DalClient.FetchAlerterConfig(alerterKey)
	if err != nil {
		log.Errorf("Unable to fetch alerter config for message %v: %v", msg.uuid, err.Error())
		return nil, err
	}

	// try to unmarshal the data
	var alerterConfig *AlerterConfig

	if err := json.Unmarshal([]byte(jsonAlerterConfig), &alerterConfig); err != nil {
		log.Errorf("Unable to unmarshal alerter config for message %v: %v", msg.uuid, err.Error())
		return nil, err
	}

	// check if we have given alerter
	if _, ok := a.Alerters[alerterConfig.Type]; !ok {
		err := fmt.Errorf("Unable to find any alerter named %v", alerterConfig.Type)
		log.Error(err.Error())
		return nil, err
	}

	return alerterConfig, nil
}

// Perform message validation; return err if one of required fields is not filled out
func (a *Alerter) validateMessage(msg *Message) error {
	if len(msg.Key) == 0 {
		return errors.New("Message must have at least one element in the 'Key' slice")
	}

	if msg.Source == "" {
		return errors.New("Message must have the 'Source' value filled out")
	}

	if msg.Contents == nil {
		return errors.New("Message 'Contents' must be filled out")
	}

	validTypes := []string{"resolve", "critical", "warning"}

	if !util.StringSliceContains(validTypes, msg.Type) {
		return fmt.Errorf("Message 'Type' must contain one of %v", validTypes)
	}

	return nil
}
