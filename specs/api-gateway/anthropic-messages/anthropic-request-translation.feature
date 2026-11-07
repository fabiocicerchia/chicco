@domain:api-gateway @area:anthropic-messages @from:proxy/anthropic-request
Feature: Accepting Anthropic Messages requests for any provider
  Lets Anthropic-SDK callers send /v1/messages to chicco and have the conversation, tools and settings reach whichever provider in the pool answers, so they never need to know the pool speaks OpenAI underneath.

  Background:
    Given chicco is running with at least one provider in its pool

  @id:api-gateway.anthropic-messages.005 @rule:anthropic-messages.request.invalid-request-rejected @inferred @source:internal/proxy/anthropic_request.go#anthropicToOpenAI
  Scenario Outline: A malformed Messages request is refused with an Anthropic error
    Given a client sends a request whose body is <problem>
    When the client posts it to /v1/messages
    Then the response status is 400
    And the body is an Anthropic error envelope with type "error" and error.type "invalid_request_error"
    And the error message starts with "chicco: invalid Anthropic request:"
    And no provider is contacted

    Examples:
      | problem                                                                  |
      | not valid JSON                                                           |
      | valid JSON with an empty "messages" list                                 |
      | valid JSON with no "messages" field                                      |
      | a message whose content is neither a string nor a list of content blocks |

  @id:api-gateway.anthropic-messages.006 @rule:anthropic-messages.request.conversation-reaches-provider @verified @source:internal/proxy/anthropic_request.go#anthropicMessages @test:internal/proxy/anthropic_test.go#TestAnthropicToOpenAI @test:internal/proxy/cli_test.go#TestRunCLIViaAnthropicEndpointGetsThePrompt
  Scenario Outline: The system prompt and every conversation turn reach the provider
    Given a client sends a system prompt "be terse" as <system_form>
    And the conversation has a user turn "ping"
    When the client posts the request to /v1/messages
    Then the provider receives "be terse" as the system instruction ahead of the conversation
    And the provider receives the user turn "ping"
    And a local CLI provider receives both texts in its prompt

    Examples:
      | system_form           |
      | a plain string        |
      | a list of text blocks |

  @id:api-gateway.anthropic-messages.007 @rule:anthropic-messages.request.tool-turns-preserved @verified @source:internal/proxy/anthropic_request.go#convertAnthropicMessage @test:internal/proxy/anthropic_test.go#TestAnthropicToOpenAI
  Scenario: Tool calls and tool results in the history carry over to the provider
    Given the conversation has an assistant turn that called tool "get_weather" with id "call_1" and input {"city":"NYC"}
    And the next user turn has a tool result for "call_1" with content "42", followed by the text "thanks"
    When the client posts the request to /v1/messages
    Then the provider sees the assistant turn as a call to function "get_weather" with id "call_1" and arguments {"city":"NYC"}
    And the provider sees a tool reply for "call_1" with content "42"
    And the provider sees the user text "thanks" after the tool reply, in the original order

  @id:api-gateway.anthropic-messages.008 @rule:anthropic-messages.request.tool-choice-mapped @inferred @source:internal/proxy/anthropic_request.go#anthropicToolChoice @questionable
  Scenario Outline: The caller's tool choice is passed on to the provider
    Given the request declares a tool "get_weather" with an input schema
    And the request sets tool_choice to <anthropic_choice>
    When the client posts the request to /v1/messages
    Then the provider is offered function "get_weather" with the same description and parameter schema
    And the provider receives tool choice <provider_choice>

    Examples:
      | anthropic_choice                     | provider_choice        |
      | {"type":"auto"}                      | "auto"                 |
      | {"type":"any"}                       | "required"             |
      | {"type":"tool","name":"get_weather"} | function "get_weather" |
      | {"type":"none"}                      | "auto"                 |

  @id:api-gateway.anthropic-messages.009 @rule:anthropic-messages.request.optional-settings-forwarded-when-set @inferred @source:internal/proxy/anthropic_request.go#anthropicOptions
  Scenario Outline: Sampling and stop settings reach the provider only when the caller sets them
    Given a client sends a request that <setting_state>
    When the client posts it to /v1/messages
    Then the provider receives <provider_sees>

    Examples:
      | setting_state                                     | provider_sees                                                    |
      | sets temperature 0.2                              | temperature 0.2                                                  |
      | sets top_p 0.9                                    | top_p 0.9                                                        |
      | sets stop_sequences ["END"]                       | stop sequences ["END"]                                           |
      | sets none of temperature, top_p or stop_sequences | no temperature, top_p or stop setting, so its own defaults apply |

  @id:api-gateway.anthropic-messages.010 @rule:anthropic-messages.request.max-tokens-always-sent @verified @source:internal/proxy/anthropic_request.go#anthropicToOpenAI @test:internal/proxy/anthropic_test.go#TestAnthropicToOpenAI @questionable
  Scenario Outline: The token limit is always forwarded, even when the caller leaves it out
    Given a client sends a request with <max_tokens_field>
    When the client posts it to /v1/messages
    Then the request is accepted
    And the provider receives a max_tokens limit of <limit>

    Examples:
      | max_tokens_field    | limit |
      | max_tokens 50       | 50    |
      | no max_tokens field | 0     |

  @id:api-gateway.anthropic-messages.011 @rule:anthropic-messages.request.non-text-blocks-dropped @inferred @source:internal/proxy/anthropic_request.go#userMessages @questionable
  Scenario: Content other than text, tool calls and tool results is silently left out
    Given the conversation has a user turn with an image block and the text "what is this?"
    When the client posts the request to /v1/messages
    Then the request is accepted
    And the provider receives only the text "what is this?" for that turn
    And the image is never sent to the provider and the caller gets no warning
