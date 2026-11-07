@domain:provider-health @area:health-endpoint @from:proxy/proxy
Feature: Reporting proxy health and live status
  Gives operators and orchestrators one probe that stays up while naming each provider's state, plus a JSON snapshot that drives the web dashboard.

  Background:
    Given chicco is running with a pool of configured providers

  @id:provider-health.health-endpoint.001 @rule:provider-health.health.always-200-with-provider-states @verified @source:internal/proxy/proxy.go#handleHealth @test:internal/proxy/proxy_test.go#TestHealthBody
  Scenario Outline: The health probe always answers 200 and says whether any provider can serve
    Given provider "good" is <good_state>
    And provider "bad" is <bad_state>
    When the orchestrator calls GET /health
    Then the response status is 200
    And the body field "status" is "<overall>"
    And the body field "providers" maps "good" to "<good_shown>" and "bad" to "<bad_shown>"

    Examples:
      | good_state                                  | bad_state             | overall  | good_shown | bad_shown |
      | healthy                                     | rejecting its API key | ok       | ok         | auth      |
      | down                                        | rejecting its API key | degraded | down       | auth      |
      | not yet probed                              | down                  | ok       | unknown    | down      |
      | healthy but cooling down after a rate limit | down                  | degraded | cooldown   | down      |

  @id:provider-health.health-endpoint.002 @rule:provider-health.health.unconfigured-providers-omitted @gap @source:internal/proxy/proxy.go#handleHealth
  Scenario: Providers with no API key or no models are left out of the health report
    Given provider "spare" is configured without an API key
    And provider "main" is healthy
    When the orchestrator calls GET /health
    Then the body field "providers" lists "main"
    And the body field "providers" does not list "spare"
