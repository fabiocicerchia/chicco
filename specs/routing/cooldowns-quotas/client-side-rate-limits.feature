@domain:routing @area:cooldowns-quotas @from:proxy/ratelimit
Feature: Respecting provider rate limits
  Operators set per-provider request and token limits so chicco stops sending traffic to a provider before the provider starts rejecting it, and moves the traffic elsewhere until the limit window clears.

  Background:
    Given a provider "groq" is in the pool with a configured quota

  @id:routing.cooldowns-quotas.001 @rule:routing.rate-limits.limit-reached-benches-provider @verified @source:internal/proxy/ratelimit.go#check @test:internal/proxy/window_test.go#TestRateLimitRPM @test:internal/proxy/window_test.go#TestRateLimitTPM
  Scenario Outline: A provider that has reached a rate limit is benched until its oldest counted request leaves the window
    Given groq has a <limit> of <value>
    And groq has served <usage> within the last <window>
    When a caller sends a chat completion request
    Then groq is not chosen for the request
    And groq stays in cooldown with reason "limit" until its oldest request in that <window> is one <window> old
    And the request goes to the next available provider

    Examples:
      | limit | value   | usage          | window   |
      | RPM   | 2       | 2 requests     | minute   |
      | RPH   | 100     | 100 requests   | hour     |
      | RPD   | 1000    | 1000 requests  | 24 hours |
      | TPM   | 100     | 110 tokens     | minute   |
      | TPH   | 50000   | 50000 tokens   | hour     |
      | TPD   | 1000000 | 1000000 tokens | 24 hours |

  @id:routing.cooldowns-quotas.002 @rule:routing.rate-limits.under-limit-served @verified @source:internal/proxy/ratelimit.go#check @test:internal/proxy/window_test.go#TestRateLimitNotTriggered
  Scenario: A provider under all of its limits keeps serving requests
    Given groq has an RPM of 10 and a TPM of 1000
    And groq has served 1 request using 50 tokens in the last minute
    When a caller sends a chat completion request
    Then groq is not put in cooldown
    And groq can serve the request

  @id:routing.cooldowns-quotas.003 @rule:routing.rate-limits.latest-reset-wins @inferred @source:internal/proxy/ratelimit.go#check
  Scenario: When several limits are reached at once, the provider stays benched until the last of them clears
    Given groq has an RPM of 2 and an RPH of 3
    And groq has served 3 requests in the last hour, the first of them 40 minutes ago, and 2 of them in the last minute
    When a caller sends a chat completion request
    Then groq stays in cooldown until 20 minutes from now, when the hourly window clears
    And groq is not back in rotation as soon as the minute window clears

  @id:routing.cooldowns-quotas.004 @rule:usage.quota-window.usage-counted-per-window @verified @source:internal/proxy/ratelimit.go#windowTotals @test:internal/proxy/window_test.go#TestEventLogDailyWindow @test:internal/proxy/window_test.go#TestEventLogNoneWindow @questionable
  Scenario Outline: Quota use is counted over the provider's configured quota window
    Given groq's quota window is <window>
    And groq served 100 tokens at 23:50 UTC yesterday and 150 tokens at 00:20 UTC today
    And it is now 00:30 UTC
    When the operator views groq's quota use
    Then groq's used tokens show <used>

    Examples:
      | window   | used |
      | daily    | 150  |
      | hourly   | 250  |
      | minutely | 0    |
      | none     | 250  |

  @id:routing.cooldowns-quotas.005 @rule:routing.rate-limits.limits-survive-restart @verified @source:internal/proxy/ratelimit.go#loadSlice @test:internal/proxy/window_test.go#TestLoadSliceDropsOldEvents @questionable
  Scenario: Rate-limit usage from the last day survives a restart
    Given groq has an RPD of 1000
    And groq served 1000 requests over the last 20 hours
    And the operator has a state file configured
    When the operator restarts chicco
    Then groq is still benched for its daily limit
    And requests older than 25 hours no longer count toward any limit or quota use

  @id:routing.cooldowns-quotas.006 @rule:routing.rate-limits.history-capped @inferred @source:internal/proxy/ratelimit.go#maxEvents @questionable
  Scenario: Only the most recent 10,000 requests per provider count toward its limits
    Given groq has an RPD of 20000
    And groq has served 15000 requests in the last 24 hours
    When a caller sends a chat completion request
    Then groq is not benched for its daily limit
    And groq's daily request count shows at most 10000
