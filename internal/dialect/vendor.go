package dialect

// Vendor is what a person needs to decide whether to use a review bot: what it
// is, what it costs, and what setting it up involves.
//
// It lives in dialect for the same reason every other bot string does — this is
// the one package allowed to know a bot by name. Nothing here affects a
// decision crq makes; it is read only by the guide.
//
// Prices carry no figures that are not in pricing.go, and no claim is made
// about which bot is better. The honest version of "which should I use" is a
// description of each and the criterion that distinguishes them.
type Vendor struct {
	// Site is where you sign up; Docs is where the setup instructions are.
	Site string
	Docs string
	// Pitch is two or three lines, in plain terms, with no superlatives.
	Pitch string
	// Cost is one line about money, in the vendor's own terms.
	Cost string
	// Setup is what installing it actually involves, in order.
	Setup []string
	// SuitedTo names the case this bot is the obvious answer for, and is shown
	// as the reason behind any suggestion — a badge with no stated criterion is
	// an advertisement.
	SuitedTo string
}

// PrimaryVendor describes the configured primary. It is keyed by login rather
// than held on a registry entry, because the primary is deliberately NOT in the
// co-reviewer registry.
func PrimaryVendor(login string) (Vendor, bool) {
	if NormalizeBotName(login) != NormalizeBotName(CodeRabbitLogin) {
		return Vendor{}, false
	}
	return Vendor{
		Site: "https://coderabbit.ai",
		Docs: "https://docs.coderabbit.ai",
		Pitch: "Reviews the whole pull request and leaves line comments, then keeps " +
			"answering as you push. It is the reviewer crq queues around: its allowance " +
			"is per-account and shared, which is why one review at a time fires fleet-wide.",
		Cost: "Free for public repositories. Paid per developer for private ones, with " +
			"usage-based overage past the plan's allowance — see crq cost.",
		Setup: []string{
			"Sign in at coderabbit.ai with GitHub and authorise the app.",
			"Add the repositories you want reviewed.",
			"Optionally commit a .coderabbit.yaml — note an org-level config overrides it.",
		},
		SuitedTo: "open-source repositories, where it is free and covers the whole diff",
	}, true
}

// CodeRabbitLogin is the default primary's login. Named here so the guide can
// ask about it without the caller hardcoding a string.
const CodeRabbitLogin = "coderabbitai[bot]"

// vendorFor returns the descriptor for a registry co-reviewer.
func vendorFor(name string) Vendor {
	switch name {
	case "codex":
		return Vendor{
			Site: "https://chatgpt.com/codex",
			Docs: "https://developers.openai.com/codex/cloud/code-review",
			Pitch: "OpenAI's reviewer, driven from a ChatGPT plan rather than a per-repository " +
				"subscription. It reads the diff in the context of the whole repository and " +
				"tends to find the defects that need that context.",
			Cost: "Included with a ChatGPT Plus/Pro/Business plan — no per-review charge, so " +
				"crq never queues it behind the shared allowance.",
			Setup: []string{
				"Connect your GitHub account in ChatGPT's Codex settings.",
				"Enable code review for the repositories you want it on.",
			},
			SuitedTo: "anyone already paying for ChatGPT — it costs nothing further",
		}
	case "bugbot":
		return Vendor{
			Site: "https://cursor.com/bugbot",
			Docs: "https://docs.cursor.com/bugbot",
			Pitch: "Cursor's reviewer. Narrower than a full review by design: it looks for " +
				"bugs rather than commenting on style, and posts a check run even when it " +
				"finds nothing.",
			Cost: "Part of a Cursor subscription; no per-review charge.",
			Setup: []string{
				"Install the Cursor GitHub app on the repositories you want it on.",
				"Enable Bugbot for them in Cursor's dashboard.",
			},
			SuitedTo: "teams already using Cursor",
		}
	case "macroscope":
		return Vendor{
			Site: "https://macroscope.com",
			Docs: "https://docs.macroscope.com",
			Pitch: "Reviews against rules you write into the repository — .macroscope/ holds " +
				"correctness checks and per-check agents — so it enforces this project's " +
				"conventions rather than generic ones.",
			Cost: "Pay per kilobyte of diff reviewed, with a per-review minimum and cap. It " +
				"is the one co-reviewer that spends money per review; crq estimates it.",
			Setup: []string{
				"Sign up at macroscope.com and install its GitHub app.",
				"Add .macroscope/ rules to the repository's default branch.",
				"Top up the balance — work is SKIPPED, not queued, when it runs out.",
			},
			SuitedTo: "repositories with conventions worth enforcing in writing",
		}
	case "greptile":
		return Vendor{
			Site: "https://www.greptile.com",
			Docs: "https://www.greptile.com/docs",
			Pitch: "Indexes the whole repository and reviews a pull request against it, " +
				"leaving prioritised line comments and a summary with a confidence score. " +
				"It reviews once when a pull request opens unless told to review every push.",
			Cost: "Prepaid credits at $1 each: a Base review is 1, Plus 3, Apex 10. Starter " +
				"includes 50 a month and open-source projects can use it free.",
			Setup: []string{
				"Sign in at greptile.com with GitHub and install its app.",
				"Enable the repositories you want reviewed and let it index them.",
				"Optionally commit a greptile.json — set triggerOnUpdates to review every push.",
			},
			SuitedTo: "large codebases where a finding depends on code outside the diff",
		}
	}
	return Vendor{}
}

// VendorFor returns the descriptor for any bot crq knows, primary or
// co-reviewer, and whether there is one.
func VendorFor(nameOrLogin string) (Vendor, bool) {
	if v, ok := PrimaryVendor(nameOrLogin); ok {
		return v, true
	}
	if co, ok := CoReviewerByName(nameOrLogin); ok {
		v := vendorFor(co.Name)
		return v, v.Site != ""
	}
	return Vendor{}, false
}
