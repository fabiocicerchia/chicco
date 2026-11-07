@domain:api-gateway @area:model-catalog @from:proxy/route
Feature: Listing only virtual models that can answer
  Lets API callers pick from /v1/models without choosing a virtual model whose every backend is known to be dead, which would only get them a 502 or 503.

  Background:
    Given chicco is running with virtual models configured in chicco.yaml

  @id:api-gateway.model-catalog.001 @rule:catalog.models.hide-dead-virtual-models @verified @source:internal/proxy/route.go#servable @test:internal/proxy/proxy_test.go#TestModelsEndpointHidesDeadModels
  Scenario Outline: A virtual model is listed only while at least one of its backends could answer
    Given the virtual model "coder" is backed by the providers "groq" and "openrouter"
    And "groq" is <groq_state>
    And "openrouter" is <openrouter_state>
    When a client lists the models at GET /v1/models
    Then "coder" <listed> in the response data

    Examples:
      | groq_state | openrouter_state                | listed        |
      | down       | rejecting its API key           | is not listed |
      | down       | down                            | is not listed |
      | down       | healthy                         | is listed     |
      | down       | cooling down after a rate limit | is listed     |
      | down       | not yet probed                  | is listed     |

  @id:api-gateway.model-catalog.002 @rule:catalog.models.unmatched-backends-stay-listed @inferred @source:internal/proxy/route.go#servable @questionable
  Scenario: A virtual model whose backends match no usable provider stays listed
    Given the virtual model "coder" names only the backend provider "groqq"
    And no active provider is named "groqq"
    When a client lists the models at GET /v1/models
    Then "coder" is listed in the response data
