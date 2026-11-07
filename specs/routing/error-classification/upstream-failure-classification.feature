@domain:routing @area:error-classification @from:proxy/classify
Feature: Benching a provider after an upstream failure
  Operators rely on chicco to decide, from each failed upstream reply, how long to skip a provider (or only one of its models) and what to call the failure, so one bad request or model id does not take healthy capacity offline.

  Background:
    Given a provider "cloudy" serving the models "good-model" and "bad-model"

  @id:routing.error-classification.001 @rule:routing.error-classification.rejected-key-benches-provider-for-an-hour @verified @source:internal/proxy/classify.go#classifyUpstream @test:internal/proxy/proxy_test.go#TestClassifyUpstream @test:internal/proxy/proxy_test.go#TestCooldown
  Scenario Outline: A rejected API key benches the whole provider for an hour
    Given the upstream reply carries no quota or model wording
    When cloudy answers a request with HTTP <status> "<message>"
    Then cloudy is skipped for every model for 1 hour
    And the failure is labelled "auth"
    And cloudy is shown as having an invalid key on the dashboard

    Examples:
      | status | message         |
      | 401    | Invalid API key |
      | 403    | Forbidden       |

  @id:routing.error-classification.002 @rule:routing.error-classification.exhausted-quota-is-a-limit-not-a-bad-key @verified @source:internal/proxy/classify.go#classifyUpstream @test:internal/proxy/proxy_test.go#TestClassifyUpstream
  Scenario Outline: An exhausted quota or credit balance benches the provider as limited, not as a bad key
    Given the upstream sends <retry_after>
    When cloudy answers a request with HTTP <status> "<message>"
    Then cloudy is skipped for every model for <bench>
    And the failure is labelled "limit"
    And cloudy is not shown as having an invalid key

    Examples:
      | status | message                                                                               | retry_after      | bench      |
      | 402    | This request requires more credits, or fewer max_tokens.                              | no Retry-After   | 1 hour     |
      | 403    | The free quota has been exhausted. To continue accessing the model on a paid basis... | no Retry-After   | 1 hour     |
      | 402    | Insufficient balance                                                                  | Retry-After: 600 | 10 minutes |

  @id:routing.error-classification.003 @rule:routing.error-classification.model-rejection-benches-only-that-model @verified @source:internal/proxy/classify.go#classifyUpstream @test:internal/proxy/proxy_test.go#TestClassifyUpstream @test:internal/proxy/proxy_test.go#TestModelScopedBlockKeepsSiblingsRoutable @questionable
  Scenario Outline: A provider rejecting one model by name keeps serving its other models
    Given the upstream reply names the model as unavailable
    When cloudy answers a request for "bad-model" with HTTP <status> "<message>"
    Then only "bad-model" on cloudy is skipped, for 5 seconds
    And the failure is labelled "error"
    And requests for "good-model" are still routed to cloudy
    And cloudy is not shown as having an invalid key

    Examples:
      | status | message                                                                              |
      | 403    | AiError: Model @cf/moonshotai/kimi-k2.7-code is not available on the Workers AI plan |
      | 404    | The model `qwen3-32b` does not exist or you do not have access to it.                |
      | 400    | Invalid model: pixtral-large-latest                                                  |

  @id:routing.error-classification.004 @rule:routing.error-classification.rejected-request-benches-provider-briefly @verified @source:internal/proxy/classify.go#cooldown @test:internal/proxy/proxy_test.go#TestCooldown
  Scenario Outline: A request the provider refuses as malformed only briefly benches the provider
    Given the upstream reply carries no quota or model wording
    And the upstream sends Retry-After: 30
    When cloudy answers a request with HTTP <status>
    Then cloudy is skipped for 5 seconds
    And the failure is labelled "error"

    Examples:
      | status |
      | 400    |
      | 404    |
      | 413    |
      | 422    |

  @id:routing.error-classification.005 @rule:routing.error-classification.transient-failure-honours-retry-after @verified @source:internal/proxy/classify.go#cooldown @test:internal/proxy/proxy_test.go#TestCooldown @test:internal/proxy/proxy_test.go#TestClassifyUpstream @questionable
  Scenario Outline: A rate limit or server failure benches the provider for the upstream's Retry-After, else one minute
    Given the upstream sends <retry_after>
    When cloudy answers a request with HTTP <status>
    Then cloudy is skipped for every model for <bench>
    And the failure is labelled "<label>"

    Examples:
      | status | retry_after                                | bench      | label |
      | 429    | Retry-After: 30                            | 30 seconds | limit |
      | 429    | no Retry-After                             | 1 minute   | limit |
      | 429    | Retry-After: Wed, 21 Oct 2026 07:28:00 GMT | 1 minute   | limit |
      | 502    | no Retry-After                             | 1 minute   | error |
      | 500    | no Retry-After                             | 1 minute   | error |
