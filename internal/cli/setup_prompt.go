package cli

import (
	"bufio"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/config"
	"strconv"
	"strings"
)

func (p *prompter) setupStep(n int, title string) {
	if !p.singleArea {
		title = fmt.Sprintf("Step %d of 3 · %s", n, title)
	}
	p.renderer().block(p.style.bold(title) + "\n")
}

func (p *prompter) setupHeading(title string) {
	p.renderer().block(p.style.bold(strings.TrimSpace(title)) + "\n")
}

func (p *prompter) setupMenu(question, def string, choices ...option) (string, error) {
	return p.guidedChoice(promptModel{Question: strings.TrimSpace(question), Default: def, Primary: choices})
}

func (p *prompter) setupActions(question, def string, primary []option, secondary []actionOption) (string, error) {
	var actions []actionOption
	for _, a := range secondary {
		if a.Shortcut == "" {
			primary = append(primary, option{a.Key, a.Label})
		} else {
			actions = append(actions, a)
		}
	}
	return p.guidedChoice(promptModel{Question: strings.TrimSpace(question), Default: def, Primary: primary, Secondary: actions})
}

type setupAffirmation string

const (
	setupAffirmative      setupAffirmation = "yes"
	setupAffirmativeAlias setupAffirmation = "y"
)

func setupAffirmed(value string) bool {
	answer := setupAffirmation(value)
	return answer == setupAffirmative || answer == setupAffirmativeAlias
}

func (p *prompter) setupYesNo(question string, def bool) (bool, error) {
	key := "no"
	if def {
		key = "yes"
	}
	answer, err := p.guidedChoice(promptModel{Question: strings.TrimSpace(question), Receipt: strings.TrimSpace(strings.SplitN(question, "?", 2)[0]), Default: key, Primary: []option{{"yes", "Yes"}, {"no", "No"}}, Aliases: []option{{"y", ""}, {"n", ""}}, ResolveReceipt: func(value string) string {
		if setupAffirmed(value) {
			return "Yes"
		}
		return "No"
	}})
	return setupAffirmed(answer), err
}

func parseRetentionAnswer(value string) (int, error) {
	if strings.EqualFold(strings.TrimSpace(value), "forever") {
		return 0, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > config.MaxRetentionDays {
		return 0, fmt.Errorf("enter 1 to 36500 days, or forever")
	}
	return n, nil
}

type retentionChoice string

const (
	retentionChoiceDefault retentionChoice = "90-days"
	retentionChoiceCustom  retentionChoice = "custom"
	retentionChoiceForever retentionChoice = "forever"
)

func (p *prompter) setupRetentionDays(def int) (int, error) {
	choice := retentionChoiceCustom
	switch def {
	case 90:
		choice = retentionChoiceDefault
	case 0:
		choice = retentionChoiceForever
	}
	custom := "Custom duration"
	if choice == retentionChoiceCustom {
		custom += " (" + config.RetentionLabel(def) + ")"
	}
	legacyDays := 0
	answer, err := p.guidedChoice(promptModel{ReadAnswer: func(in *bufio.Reader) (string, error) {
		legacyDays = 0
		raw, err := in.ReadString('\n')
		// Menu indices 1–3 select the visible choices. Existing duration
		// answers such as 90 or 30 remain valid without an extra question.
		if n, parseErr := strconv.Atoi(strings.TrimSpace(raw)); parseErr == nil && n > 3 && n <= config.MaxRetentionDays {
			legacyDays = n
			return string(retentionChoiceCustom), err
		}
		return raw, err
	}, ResolveReceipt: func(key string) string {
		if legacyDays != 0 {
			return config.RetentionLabel(legacyDays)
		}
		switch retentionChoice(key) {
		case retentionChoiceDefault:
			return "90 days"
		case retentionChoiceForever:
			return "Keep forever"
		case retentionChoiceCustom:
			return custom
		}
		return key
	}, Question: "How long should sessions be kept?", Default: string(choice), Receipt: "Retention", Primary: []option{{string(retentionChoiceDefault), "90 days"}, {string(retentionChoiceCustom), custom}, {string(retentionChoiceForever), "Keep forever"}}, Helpers: []string{"Provider lifecycle rules and explicit deletion still apply."}})
	if err != nil {
		return 0, err
	}
	if legacyDays != 0 {
		return legacyDays, nil
	}
	switch retentionChoice(answer) {
	case retentionChoiceDefault:
		return 90, nil
	case retentionChoiceForever:
		return 0, nil
	case retentionChoiceCustom:
		// Ask for the duration below.
	}
	value := strconv.Itoa(def)
	if def == 0 {
		value = "forever"
	}
	answer, err = p.guidedText(promptModel{Question: "Keep sessions for how many days?", Label: "Days or forever", Default: value, Receipt: "Retention", Validate: func(value string) error { _, err := parseRetentionAnswer(value); return err }})
	if err != nil {
		return 0, err
	}
	return parseRetentionAnswer(answer)
}

// Storage field actions use a reserved prefix so valid bucket/profile names
// remain literal, including "b" and "back".
func (p *prompter) setupStorageRequired(label, def string) (string, error) {
	return p.setupStorageField(label, def, false)
}

func (p *prompter) setupStorageField(label, def string, secret bool) (string, error) {
	answerLabel := "Answer"
	if secret {
		answerLabel = "Credential"
	}
	value, err := p.guidedText(promptModel{Question: label, Secret: secret, Helpers: []string{"[:back] Back to storage options"}, Label: answerLabel, Default: def, Receipt: label, Validate: func(value string) error {
		if value == "" {
			return fmt.Errorf("this value is required")
		}
		return nil
	}})
	if err == nil && value == ":back" {
		return "", errChooseStorageAgain
	}
	return value, err
}
