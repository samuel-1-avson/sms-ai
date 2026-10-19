package commands

import (
	"testing"

	"smsaighana/internal/formatting"
)

func TestParse(t *testing.T) {
	cases := map[string]Command{
		"help":                    {Kind: Help},
		" HELP ":                  {Kind: Help},
		"?":                       {Kind: Help},
		"more":                    {Kind: More},
		"More.":                   {Kind: More},
		"m":                       {Kind: More},
		"NEW":                     {Kind: New},
		"stop":                    {Kind: Stop},
		"Start!":                  {Kind: Start},
		"code stemgh":             {Kind: Code, Arg: "STEMGH"},
		"CODE: WASSCE2026":        {Kind: Code, Arg: "WASSCE2026"},
		"delete my data":          {Kind: Delete},
		"more examples of verbs":  {Kind: None},
		"What is photosynthesis?": {Kind: None},
		"code":                    {Kind: None},
		"stop the bleeding":       {Kind: None},
	}
	for in, want := range cases {
		if got := Parse(in); got != want {
			t.Errorf("Parse(%q) = %+v; want %+v", in, got, want)
		}
	}
}

func TestTextsFitOneSegment(t *testing.T) {
	texts := []string{
		HelpText, NewChatText, NoMoreText, StopText, StartText, DeleteText, CodeInvalidText,
		CodeExpiredText, CodeUsedText, CodeTooManyText, BusyText, FailedText, USSDThinkingText,
		LimitReachedText(15, 15), LimitReachedText(150, 150), CodeAcceptedText(100, 1000),
	}
	for _, s := range texts {
		st := formatting.Analyze(s)
		if st.Encoding != formatting.GSM7 || st.Segments != 1 {
			t.Errorf("%q is %+v; want one GSM-7 segment", s, st)
		}
	}
}
