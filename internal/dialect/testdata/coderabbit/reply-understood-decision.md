<!-- coderabbit-ai-generated:v1 -->

@kristofferR The supplied code logs per-repository exclusion lookup errors and reports them for `--once`. I understand the decision to continue processing other repositories rather than stop the whole pass.

The rename risk remains when a host-only exclusion cannot be resolved and no alias is recorded. I’m not marking the finding as addressed.

---

<details>
<summary>Learnings added</summary>
<dl>
<dd>
<br>

```
Learnt from: kristofferR
Repo: kristofferR/codereview-queue PR: 84
File: internal/crq/watch.go:267-287
Timestamp: 2026-11-28T01:27:24.114Z
Learning: Within repository "kristofferR/codereview-queue", only for files matching one of ["internal/crq/watch.go"]: In `internal/crq/watch.go`, do not require `watchPass` to stop the entire pass for a per-repository `CRQ_EXCLUDE` lookup error. The chosen tradeoff is to log the error and report it for a one-shot watch while continuing other repositories, because a persistent permission or SAML failure could otherwise stop all repositories. This accepts that a renamed host-only exclusion without a recorded alias may be acted on under its new name while the lookup fails.
```

> Note: Learnings are effective only in the context of similar code segments. To apply general coding guidelines, please configure [review instructions](https://docs.coderabbit.ai/guides/review-instructions). You can manage existing learnings in the [app](https://app.coderabbit.ai/learnings).

</dd>
</dl>
</details>

<sub><i>You are interacting with an AI system.</i></sub>

<!-- This is an auto-generated reply by CodeRabbit -->