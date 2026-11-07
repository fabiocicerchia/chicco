@domain:provider-health @area:model-testing @from:proxy/test
Feature: Testing every configured model on demand
  Lets the operator check from the terminal dashboard, without leaving chicco, which configured models really answer and where each provider stands against its quota.

  Background:
    Given the operator is running chicco with the terminal dashboard open

  @id:provider-health.model-testing.001 @rule:provider-health.model-testing.probes-every-model-of-usable-providers @inferred @source:internal/proxy/test.go#runTest
  Scenario: A model test sends one hello-world prompt to every model of every usable provider
    Given provider "groq" has an API key and models "llama-3.1-8b" and "llama-3.3-70b"
    And provider "claude-cli" is a local CLI provider with model "sonnet"
    And provider "openrouter" lists models but has no API key
    When the operator starts a model test from the terminal dashboard
    Then the log pane shows "chicco test: probing 3 model(s) with a hello-world prompt…"
    And each of the 3 models gets one prompt asking it to reply exactly "hello world", and one ✓ or ✗ line in the log
    And "openrouter" is not tested
    And the log pane ends with "chicco test: done — <answered>/3 model(s) responded"

  @id:provider-health.model-testing.002 @rule:provider-health.model-testing.bypasses-cooldown @inferred @source:internal/proxy/test.go#testOne
  Scenario: A model that is cooling down is still tested
    Given provider "groq" is benched for a rate limit with 40 minutes left
    When the operator starts a model test from the terminal dashboard
    Then every "groq" model is still sent the hello-world prompt
    And the log line for each "groq" model shows what the provider actually answered right now

  @id:provider-health.model-testing.003 @rule:provider-health.model-testing.success-restores-health-and-counts-usage @inferred @source:internal/proxy/test.go#runTest
  Scenario: A model that answers marks its provider healthy and counts toward its usage
    Given provider "groq" is shown as unhealthy
    And model "llama-3.1-8b" answers the prompt and reports 12 tokens
    When the operator starts a model test from the terminal dashboard
    Then the log pane shows "chicco test: ✓ groq/llama-3.1-8b — 12 tok · <quota window>"
    And "groq" is shown as healthy in the provider table
    And the 12 tokens and one request are added to the usage of "groq" and "llama-3.1-8b"

  @id:provider-health.model-testing.004 @rule:provider-health.model-testing.failure-benches-provider-with-reason @inferred @source:internal/proxy/test.go#runTest @questionable
  Scenario Outline: A model that fails benches its provider and the log says why
    Given provider "groq" has model "llama-3.1-8b"
    And the provider answers the prompt with status <status> <retry_after>
    When the operator starts a model test from the terminal dashboard
    Then the log pane shows "chicco test: ✗ groq/llama-3.1-8b — <detail> · <quota window>"
    And "groq" is benched for <bench> with reason "<reason>"
    And "groq" is shown with health <health>

    Examples:
      | status                        | retry_after                     | detail                                           | bench      | reason | health       |
      | 401                           | and no retry hint               | auth failed — login required                     | 1 hour     | auth   | auth failure |
      | 403                           | and no retry hint               | auth failed — login required                     | 1 hour     | auth   | auth failure |
      | 429                           | asking to retry in 30 minutes   | limit — resets in 30m0s                          | 30 minutes | limit  | unchanged    |
      | 429                           | and no retry hint               | limit reached                                    | 1 minute   | limit  | unchanged    |
      | 500                           | with body "upstream overloaded" | error: upstream overloaded                       | 1 minute   | error  | unchanged    |
      | 404                           | with body "model not found"     | error: model not found                           | 5 seconds  | error  | unchanged    |
      | no reply (connection refused) |                                 | error: <connection error, at most 60 characters> | 1 minute   | error  | down         |

  @id:provider-health.model-testing.005 @rule:provider-health.model-testing.reports-quota-window @inferred @source:internal/proxy/test.go#windowDesc
  Scenario Outline: Each test line ends with the provider's quota standing
    Given provider "groq" has <quota_setup>
    When the operator starts a model test from the terminal dashboard
    Then each "groq" line in the log ends with "<window>"

    Examples:
      | quota_setup                                                                      | window                                      |
      | a rate-limit bench with 25 minutes left and its models answer the test           | LIMIT — resets in 25m0s                     |
      | a daily quota of 500000 tokens with 12345 used, counting the test's own replies  | 12.3k/500.0k tok · daily                    |
      | an hourly quota of 100 requests with 7 made, counting the test's own requests    | 7/100 req · hourly                          |
      | no quota configured                                                              | no tracked quota (subscription / per-token) |

  @id:provider-health.model-testing.006 @rule:provider-health.model-testing.single-run-at-a-time @inferred @source:internal/proxy/test.go#runTest
  Scenario: Only one model test runs at a time
    Given a model test is already running
    When the operator starts a model test from the terminal dashboard
    Then the log pane shows "chicco test: a test run is already in progress"
    And no second set of prompts is sent
    And the test already running carries on to its "done" line

  @id:provider-health.model-testing.007 @rule:provider-health.model-testing.nothing-to-test @inferred @source:internal/proxy/test.go#runTest @questionable
  Scenario: A model test with no usable providers says so and sends nothing
    Given no provider has both a model and either an API key or a local CLI
    When the operator starts a model test from the terminal dashboard
    Then the log pane shows "chicco test: no providers configured"
    And no prompt is sent
