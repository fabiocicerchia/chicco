@domain:routing @area:cooldowns-quotas @from:proxy/route
Feature: Respecting configured rate limits before calling providers
  Lets operators cap requests and tokens per provider, per model and globally, so chicco moves on or refuses before an upstream quota is breached.

  Background:
    Given chicco is running with the providers "groq" and "openrouter" active, in that config order

  @id:routing.cooldowns-quotas.007 @rule:quotas.limits.provider-limit-benches-provider @inferred @source:internal/proxy/route.go#tripQuota
  Scenario: A provider that has reached its configured limit is skipped until its window frees up
    Given "groq" is configured with a limit of 2 requests per minute
    And "groq" has served 2 requests in the last minute
    When a client requests a completion with the model "chicco:auto"
    Then the request is served by "openrouter"
    And "groq" shows as cooling down for reason "limit" until the oldest of its 2 requests is a minute old

  @id:routing.cooldowns-quotas.008 @rule:quotas.limits.model-limit-benches-only-that-model @inferred @source:internal/proxy/route.go#pick
  Scenario: A backend model that has reached its own limit is skipped while the provider's other models still serve
    Given the virtual model "coder" lists groq/llama-3.3-70b with a limit of 10 requests per day, and groq/gemma2-9b with no limit of its own
    And "llama-3.3-70b" on "groq" has served 10 requests in the last 24 hours
    When clients send successive completion requests with the model "coder"
    Then no request is sent to "groq" with the model "llama-3.3-70b"
    And the requests that reach "groq" are served with the model "gemma2-9b"
    And "groq" itself is not cooling down

  @id:routing.cooldowns-quotas.009 @rule:quotas.limits.global-cap-refuses-all @verified @source:internal/proxy/route.go#pick @test:internal/proxy/proxy_test.go#TestGlobalQuotaCapsAcrossProviders
  Scenario: Once the global limit is reached every request is refused with 503, even though the providers are fine
    Given chicco is configured with a global limit of 2 requests per day
    And "groq" has no limit of its own
    And 2 requests have been served in the last 24 hours
    When a client requests a completion at POST /v1/chat/completions
    Then the response status is 503
