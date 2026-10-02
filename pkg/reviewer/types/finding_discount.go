package types

import (
	"os"
	"strings"
)

// DiscountGateEnabled is the kill switch for the discount-aware gate: a
// contract that discounts its own finding is capped at LOW and never goes
// inline, and a CRITICAL needs the same contract as a MEDIUM to be shown.
// PUBLISH_DISCOUNT_GATE=false restores the previous behaviour.
func DiscountGateEnabled() bool {
	return os.Getenv("PUBLISH_DISCOUNT_GATE") != "false"
}

const (
	DiscountNone        = ""
	DiscountUnfalsified = "not_falsifiable"
	DiscountNilImpact   = "nil_impact"
	DiscountHedged      = "hedged"
)

// nilImpactPhrases open a sentence that concedes there is no impact today.
var nilImpactPhrases = []string{
	"none today", "none currently", "none at present", "none right now", "none yet",
	"no user impact", "no current impact", "no impact today", "no impact currently",
	"no production impact", "no current user", "no current caller", "no current production",
	"no user-visible", "no user visible", "no user-facing", "no user facing",
	"no observable", "no runtime impact", "no behavior change", "no behaviour change",
	"not currently reachable", "not reachable today", "nothing today", "no impact",
	"currently none", "currently no", "today there is no", "there is no current",
}

// intentPhrases open a sentence that guesses the author meant it.
var intentPhrases = []string{
	"may be deliberate", "may be intentional", "may be intended", "may be by design", "may be on purpose",
	"might be deliberate", "might be intentional", "might be intended", "might be by design",
	"could be deliberate", "could be intentional", "could be intended", "could be by design",
	"likely deliberate", "likely intentional", "likely intended", "likely by design",
	"probably deliberate", "probably intentional", "probably intended", "probably by design",
	"possibly deliberate", "possibly intentional", "possibly intended", "possibly by design",
	"perhaps deliberate", "perhaps intentional", "perhaps intended",
	"seems deliberate", "seems intentional", "seems intended", "seems by design",
	"appears deliberate", "appears intentional", "appears intended", "appears to be intentional", "appears to be deliberate",
	"this may be deliberate", "this may be intentional", "this might be intentional", "this could be intentional",
	"this is likely intentional", "this is probably intentional", "this looks intentional", "this looks deliberate",
	"by design", "on purpose", "intentional", "deliberate", "intended behavior", "intended behaviour",
	"if this is intentional", "if this is deliberate", "if this is intended", "if intentional", "if deliberate", "if intended",
	"unless this is intentional", "unless this is deliberate", "unless intended",
}

// conditionPhrases open a claim the agent conditioned on something it did
// not check, or admit it could not check it.
var conditionPhrases = []string{
	"if ", "only if", "unless ", "assuming", "assumes", "assumed", "provided that", "in case ",
	"depends on", "this depends", "it depends", "that depends", "depending on", "dependent on",
	"whether", "uncertain whether", "unclear whether", "unsure whether", "not sure whether", "not certain whether",
	"uncertain if", "unclear if", "unsure if", "not sure if", "not certain if",
	"it is unclear", "it is uncertain", "it is unknown", "it's unclear", "it's uncertain", "it's unknown",
	"unknown whether", "unknown if", "not known whether", "not known if",
	"unverified", "not verified", "unconfirmed", "not confirmed", "speculative", "speculation",
	"could not verify", "could not confirm", "could not check", "could not read", "could not run", "could not execute", "could not inspect", "could not open",
	"couldn't verify", "couldn't confirm", "couldn't check", "couldn't read", "couldn't run", "couldn't execute", "couldn't inspect",
	"cannot verify", "cannot confirm", "cannot check", "can't verify", "can't confirm", "can't check",
	"did not verify", "did not confirm", "did not check", "did not read", "did not run", "did not inspect",
	"didn't verify", "didn't confirm", "didn't check", "didn't read", "didn't run", "didn't inspect",
	"i could not", "i couldn't", "i cannot", "i can't", "i was unable", "i did not", "i didn't", "i have not", "i haven't", "i was not able",
	"unable to", "was not able", "were not able", "not able to",
	"without running", "without reading", "without checking", "without verifying", "without access",
	"may not apply", "may not be reachable", "may not manifest", "may not be", "might not be", "might not apply",
	"not inspected", "not examined", "not reviewed", "not tested", "not exercised", "not reproduced",
	"inferred", "guess", "hypothesis", "presumably", "supposedly",
}

// confidenceLabels are prefixes the agent sometimes puts before the hedge
// ("Medium: depends on ..."); they are skipped before matching. Longer
// labels come first so "medium confidence:" is not cut after "medium".
var confidenceLabels = []string{
	"high confidence", "medium confidence", "moderate confidence", "low confidence",
	"confidence high", "confidence medium", "confidence low", "confidence",
	"very high", "very low", "fairly high", "fairly low", "high", "medium", "moderate", "low",
}

// SelfDiscount reports why a contract discounts its own finding, or
// DiscountNone. A finding is discounted when the agent could not state an
// experiment (falsifiability other than falsifiable), when current_impact
// opens by conceding no impact today or guessing at intent, or when
// uncertainty opens with a nil-impact, intent or unchecked-condition phrase.
// The audit found every confirmed false positive hedged this way while
// still claiming current impact. Invalid contracts are not judged here.
func SelfDiscount(c *FindingContract) string {
	if c == nil {
		return DiscountNone
	}
	if c.Falsifiability != "falsifiable" {
		return DiscountUnfalsified
	}
	impact := discountOpening(c.CurrentImpact)
	if opensWithAny(impact, nilImpactPhrases) {
		return DiscountNilImpact
	}
	if opensWithAny(impact, intentPhrases) {
		return DiscountHedged
	}
	uncertainty := discountOpening(c.Uncertainty)
	if opensWithAny(uncertainty, nilImpactPhrases) {
		return DiscountNilImpact
	}
	if opensWithAny(uncertainty, intentPhrases) || opensWithAny(uncertainty, conditionPhrases) {
		return DiscountHedged
	}
	return DiscountNone
}

// discountOpening lowercases the text, drops leading punctuation and quotes,
// and strips one confidence label such as "Medium:" or "Low confidence,".
func discountOpening(text string) string {
	s := strings.ToLower(strings.TrimSpace(text))
	s = strings.TrimLeft(s, "\"'`*_([{-: \t")
	for _, label := range confidenceLabels {
		if !strings.HasPrefix(s, label) {
			continue
		}
		rest := strings.TrimLeft(s[len(label):], " \t")
		if rest == "" || !strings.ContainsRune(":,;-", rune(rest[0])) {
			continue
		}
		if rest = strings.TrimLeft(rest, ":,;- \t"); rest != "" {
			return rest
		}
	}
	return s
}

func opensWithAny(text string, phrases []string) bool {
	if text == "" {
		return false
	}
	for _, phrase := range phrases {
		if !strings.HasPrefix(text, phrase) {
			continue
		}
		rest := text[len(phrase):]
		if strings.HasSuffix(phrase, " ") || rest == "" {
			return true
		}
		if next := rest[0]; next == ' ' || next == ',' || next == '.' || next == ';' || next == ':' || next == ')' || next == '-' {
			return true
		}
	}
	return false
}
