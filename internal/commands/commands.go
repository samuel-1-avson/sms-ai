// Package commands parses the keyword commands handled before any AI call
// (plan §6.2): HELP, NEW, MORE, STOP, START, CODE <VOUCHER> and DELETE.
package commands

import (
	"fmt"
	"regexp"
	"strings"
)

type Kind string

const (
	None   Kind = ""
	Help   Kind = "HELP"
	New    Kind = "NEW"
	More   Kind = "MORE"
	Stop   Kind = "STOP"
	Start  Kind = "START"
	Code   Kind = "CODE"
	Delete Kind = "DELETE"
)

type Command struct {
	Kind Kind
	Arg  string
}

var (
	reCode     = regexp.MustCompile(`^CODE[\s:#-]*([A-Z0-9-]{3,24})$`)
	reTrailing = regexp.MustCompile(`[\s.!?]+$`)
)

var keywords = map[string]Kind{
	"HELP": Help, "?": Help, "INFO": Help, "MENU": Help,
	"NEW": New, "RESET": New, "NEW CHAT": New,
	"MORE": More, "M": More,
	"STOP": Stop, "STOPALL": Stop, "UNSUBSCRIBE": Stop, "CANCEL": Stop, "OPTOUT": Stop, "OPT OUT": Stop,
	"START": Start, "UNSTOP": Start, "SUBSCRIBE": Start,
	"DELETE": Delete, "DELETE MY DATA": Delete, "FORGET ME": Delete,
}

// Parse classifies a whole message. Commands must be the entire message, so
// "more examples of verbs" is a question, not the MORE command.
func Parse(body string) Command {
	s := strings.ToUpper(strings.TrimSpace(body))
	s = strings.Join(strings.Fields(s), " ")
	if s != "?" {
		s = reTrailing.ReplaceAllString(s, "")
	}
	if k, ok := keywords[s]; ok {
		return Command{Kind: k}
	}
	if m := reCode.FindStringSubmatch(s); m != nil {
		return Command{Kind: Code, Arg: m[1]}
	}
	return Command{Kind: None}
}

// User-facing texts. Every one must stay within a single GSM-7 segment
// (enforced by tests).
const (
	HelpText = "SMS AI Ghana: text any question. MORE=rest, NEW=new chat, CODE x=study pass, STOP=opt out. " +
		"AI can be wrong. Chats kept 30 days. SMS rates may apply."
	NewChatText          = "New chat started. Ask me anything."
	NoMoreText           = "Nothing more for your last answer. Send a new question anytime."
	StopText             = "You will get no more messages from SMS AI Ghana. Text START to rejoin."
	StartText            = "Welcome to SMS AI Ghana! Text any question. Text HELP for commands."
	DeleteText           = "Your chat history has been deleted. Text any question to start again."
	CodeInvalidText      = "That code is not valid. Check it and try again, e.g. CODE STEMGH."
	CodeExpiredText      = "That code has expired or its pool is used up. Ask your school or sponsor for a new one."
	CodeUsedText         = "You have already used that code on this number."
	CodeTooManyText      = "Too many wrong codes today. Please try again tomorrow."
	BusyText             = "SMS AI is busy right now. Please try again in a few minutes."
	FailedText           = "Sorry, I could not answer that right now. Please try again shortly."
	USSDSMSUnavailable   = "SMS answers are paused right now. Please try again later."
	USSDOptedOutText     = "You opted out of SMS AI Ghana."
	USSDThinkingText     = "Thinking... Your answer will arrive by SMS in a few seconds."
	USSDSavedText        = "Sent! Check your SMS inbox."
	USSDGoodbyeText      = "Thank you for using SMS AI Ghana."
	USSDEmptyQuestion    = "Please type a question."
	USSDNothingSavedText = "Nothing saved from your last answer."
)

// LimitReachedText is the one-time daily-cap notice (plan §5.4).
func LimitReachedText(used, limit int) string {
	return fmt.Sprintf("Daily limit reached (%d/%d). Reply tomorrow or text CODE <voucher> if you have a school/study pass.", used, limit)
}

// CodeAcceptedText confirms a voucher redemption.
func CodeAcceptedText(granted, balance int) string {
	return fmt.Sprintf("Code accepted: %d extra questions added. Bonus balance: %d.", granted, balance)
}
