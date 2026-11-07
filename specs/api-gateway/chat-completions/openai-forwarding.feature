@domain:api-gateway @area:chat-completions @from:proxy/proxy
Feature: Forwarding OpenAI chat and embeddings requests
  Lets OpenAI-compatible clients send chat completions and embeddings to one endpoint and get a provider's reply as if they had called it directly.

  Background:
    Given chicco has at least one healthy HTTP provider configured with an API key and models

  @id:api-gateway.chat-completions.001 @rule:api-gateway.openai.invalid-requests-rejected @gap @source:internal/proxy/proxy.go#readPostedJSON @questionable
  Scenario Outline: A request that is not a JSON POST is refused the same way on chat and embeddings
    Given the client targets <path>
    When the client sends <request>
    Then the response status is <status>
    And the body is <body>

    Examples:
      | path                 | request                             | status | body                                                                                         |
      | /v1/chat/completions | a GET                               | 405    | the plain text "method not allowed"                                                          |
      | /v1/embeddings       | a GET                               | 405    | the plain text "method not allowed"                                                          |
      | /v1/chat/completions | a POST whose body is not valid JSON | 400    | an error object with type "chicco_error" and a message starting "chicco: invalid JSON body:" |
      | /v1/embeddings       | a POST whose body is not valid JSON | 400    | an error object with type "chicco_error" and a message starting "chicco: invalid JSON body:" |

  @id:api-gateway.chat-completions.002 @rule:api-gateway.openai.exhausted-503-with-retry-after @verified @source:internal/proxy/proxy.go#setRetryAfter @test:internal/proxy/proxy_test.go#TestExhaustedRetryAfter
  Scenario Outline: When every provider is cooling down the caller is told when to retry
    Given the only provider is cooling down for 90 more seconds after exhausting its quota
    When the client posts a request to <path>
    Then the response status is 503
    And the response carries a "Retry-After" header of whole seconds between 1 and 90, rounded up and never 0
    And the error message starts with "chicco: all providers exhausted"

    Examples:
      | path                 |
      | /v1/chat/completions |
      | /v1/messages         |
