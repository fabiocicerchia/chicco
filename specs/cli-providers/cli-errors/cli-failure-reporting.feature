@domain:cli-providers @area:cli-errors @from:proxy/clierrors
Feature: Reporting why a CLI provider failed
  Lets operators tell a logged-out or rate-limited local CLI tool from a passing glitch, so the provider is benched for the right length of time and the caller sees the real reason.

  Background:
    Given a CLI provider "gemini-cli" is in the provider pool
    And another provider in the pool can answer the request

  @id:cli-providers.cli-errors.001 @rule:cli-providers.errors.logged-out-tool-greyed-out @verified @source:internal/proxy/clierrors.go#cliFailure @test:internal/proxy/cli_test.go#TestCLIFailureClassifies @test:internal/proxy/cli_test.go#TestCLIFailureKeepsTheReason
  Scenario Outline: A logged-out CLI tool is marked as an authentication failure
    Given the CLI tool fails and prints "<message>"
    When a caller sends a chat completion request routed to "gemini-cli"
    Then "gemini-cli" is shown greyed out as an authentication failure on the dashboard
    And "gemini-cli" is benched for one hour
    And the request fails over to the next provider

    Examples:
      | message                                                 |
      | Not logged in · Please run /login                       |
      | invalid api key                                         |
      | IneligibleTierError: This client is no longer supported |
      | Error authenticating: 401                               |

  @id:cli-providers.cli-errors.002 @rule:cli-providers.errors.usage-limit-benched-until-reset @verified @source:internal/proxy/clierrors.go#parseResetDuration @test:internal/proxy/cli_test.go#TestCLIFailureClassifies @test:internal/proxy/cli_test.go#TestParseResetDuration
  Scenario Outline: A rate-limited CLI tool is benched until its usage window reopens
    Given the CLI tool fails and prints "<message>"
    When a caller sends a chat completion request routed to "gemini-cli"
    Then "gemini-cli" is benched as over its limit for <bench>
    And the request fails over to the next provider

    Examples:
      | message                                       | bench              |
      | 5-hour limit reached ∙ resets in 2h 30m       | 2 hours 30 minutes |
      | Rate limit hit, try again in 45 minutes       | 45 minutes         |
      | Quota exceeded, available again in 30 seconds | 30 seconds         |
      | You have reached your usage limit.            | 1 hour             |

  @id:cli-providers.cli-errors.003 @rule:cli-providers.errors.clock-reset-next-occurrence @verified @source:internal/proxy/clierrors.go#clockReset @test:internal/proxy/cli_test.go#TestParseResetDuration
  Scenario Outline: A reset given as a clock time benches the tool until that time next comes round
    Given it is 16:00 local time
    And the CLI tool fails and prints "usage limit, resets at <clock>"
    When a caller sends a chat completion request routed to "gemini-cli"
    Then "gemini-cli" is benched as over its limit until <until>

    Examples:
      | clock | until          |
      | 5pm   | 17:00 today    |
      | 3pm   | 15:00 tomorrow |
      | 18:30 | 18:30 today    |
      | 12am  | 00:00 tomorrow |

  @id:cli-providers.cli-errors.004 @rule:cli-providers.errors.other-failures-transient @verified @source:internal/proxy/clierrors.go#cliFailure @test:internal/proxy/cli_test.go#TestCLIFailureClassifies
  Scenario: Any other CLI failure counts as a passing glitch
    Given the CLI tool fails and prints "dial tcp: connection refused"
    When a caller sends a chat completion request routed to "gemini-cli"
    Then "gemini-cli" is benched for one minute
    And "gemini-cli" is not greyed out on the dashboard
    And the request fails over to the next provider

  @id:cli-providers.cli-errors.005 @rule:cli-providers.errors.reason-kept-in-error-reply @verified @source:internal/proxy/clierrors.go#cliFailure @test:internal/proxy/cli_test.go#TestCLIFailureKeepsTheReason
  Scenario: The caller's error keeps the CLI's real failure reason, not its stack trace
    Given every other provider in the pool is already benched
    And the CLI tool prints a warning, a long stack trace, then "This client is no longer supported"
    When a caller sends a chat completion request routed to "gemini-cli"
    Then the caller receives status 503
    And the error names "gemini-cli: HTTP 401" and includes "This client is no longer supported"
    And the error contains no stack-trace "at …" lines
    And when the remaining output is longer than 512 bytes, the reason shown is a leading "…" plus the end of the output, cut to 512 bytes in all (so its final 3 bytes are lost)

  @id:cli-providers.cli-errors.006 @rule:cli-providers.errors.broad-wording-misclassifies @inferred @source:internal/proxy/clierrors.go#cliFailure @questionable
  Scenario Outline: Ordinary retry or expiry wording in CLI output is read as a limit or a logout
    Given the CLI tool fails and prints "<message>"
    When a caller sends a chat completion request routed to "gemini-cli"
    Then "gemini-cli" is benched as <outcome>

    Examples:
      | message                                 | outcome                                          |
      | Network hiccup, please try again        | over its limit for 1 hour                        |
      | Request timeout expired                 | an authentication failure, greyed out for 1 hour |
      | Rate limit reached; sign in to raise it | an authentication failure, greyed out for 1 hour |
