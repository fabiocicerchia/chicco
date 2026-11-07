@domain:usage-and-cost @area:quota-alerts @from:proxy/alert
Feature: Warning before a provider's quota runs out
  Lets operators hear that a provider is nearly out of quota, through a log line and an optional webhook, before the rotation quietly drains the whole pool.

  Background:
    Given the operator has set alerts.threshold_percent to 80 in chicco.yaml
    And provider "groq" has a daily quota of 1000 tokens

  @id:usage-and-cost.quota-alerts.001 @rule:usage.quota-alerts.rearm-when-window-rolls @verified @source:internal/proxy/alert.go#windowStart @test:internal/proxy/alert_test.go#TestResetsWhenTheWindowRollsOver @test:internal/proxy/alert_test.go#TestHourlyAndMinutelyWindowsReArmOnTheirOwnBoundary @test:internal/proxy/alert_test.go#TestAQuotaThatNeverResetsAlertsOnce @questionable
  Scenario Outline: A quota warning can be raised again once the provider's quota window rolls over
    Given provider "p" has a <window> quota and has already been warned about
    And "p" is still over its threshold
    When a request served by "p" is recorded <later>
    Then <outcome>

    Examples:
      | window                                       | later                       | outcome                             |
      | daily                                        | 6 hours later, same UTC day | no new warning is logged            |
      | daily                                        | on the next UTC day         | a new quota warning is logged for p |
      | hourly                                       | in the next clock hour      | a new quota warning is logged for p |
      | minutely                                     | in the next clock minute    | a new quota warning is logged for p |
      | unlimited-window (no rate window configured) | three days later            | no new warning is logged            |

  @id:usage-and-cost.quota-alerts.002 @rule:usage.quota-alerts.webhook-payload-has-no-request-content @verified @source:internal/proxy/alert.go#Alert @test:internal/proxy/alert_test.go#TestWebhookPayloadCarriesNoRequestContent
  Scenario: A quota webhook carries only usage figures, never request content or keys
    Given the operator has set alerts.webhook to https://hooks.example.com/chicco
    When groq crosses its alert threshold
    Then the webhook receives one JSON POST with Content-Type application/json
    And its body has only the fields provider, threshold_percent, used, quota, percent, unit, window and at
    And no prompt, completion or API key appears in it

  @id:usage-and-cost.quota-alerts.003 @rule:usage.quota-alerts.watches-largest-window-only @verified @source:internal/proxy/config.go#quotaBar @test:internal/proxy/alert_test.go#TestCheckBudgetsReadsTheProvidersOwnWindow @questionable
  Scenario: Only a provider's largest configured quota window is watched for warnings
    Given provider "groq" also has a per-minute limit of 30 requests
    And groq has used 30 requests this minute and 500 tokens today
    When a request served by groq is recorded
    Then no quota warning is logged
    And a warning is logged only when groq reaches 80% of its 1000 daily tokens
