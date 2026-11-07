@domain:api-gateway @area:anthropic-messages @from:proxy/anthropic-json
Feature: Answering non-streaming Anthropic messages
  Callers using the Anthropic SDK without streaming get one complete Anthropic message object, whichever OpenAI-compatible provider answered, so their client code works without changes.

  Background:
    Given chicco serves the Anthropic messages endpoint at /v1/messages
    And a provider in the pool answers the request

  @id:api-gateway.anthropic-messages.001 @rule:api-gateway.anthropic-messages.single-message-object @verified @source:internal/proxy/anthropic_json.go#respondAnthropicJSON @test:internal/proxy/anthropic_test.go#TestMessagesNonStreaming
  Scenario: A non-streaming request gets one complete Anthropic message
    Given the provider replies with the text "hi" and finishes normally
    When a client sends POST /v1/messages without "stream": true
    Then the response status is 200 with Content-Type application/json
    And the body has "type": "message" and "role": "assistant"
    And "content" holds exactly one block of type "text" with the text "hi"
    And "stop_reason" is "end_turn" and "stop_sequence" is null

  @id:api-gateway.anthropic-messages.002 @rule:api-gateway.anthropic-messages.stop-reason-mapping @verified @source:internal/proxy/anthropic_stream.go#mapStopReason @test:internal/proxy/anthropic_test.go#TestMessagesNonStreaming @test:internal/proxy/anthropic_test.go#TestMessagesToolUse @questionable
  Scenario Outline: The provider's finish reason is reported as the matching Anthropic stop reason
    Given the provider finishes its reply with finish reason <finish_reason>
    When a client sends POST /v1/messages without "stream": true
    Then "stop_reason" is "<stop_reason>"

    Examples:
      | finish_reason  | stop_reason   |
      | stop           | end_turn      |
      | length         | max_tokens    |
      | tool_calls     | tool_use      |
      | content_filter | stop_sequence |
      | (none given)   | end_turn      |

  @id:api-gateway.anthropic-messages.003 @rule:api-gateway.anthropic-messages.message-id-and-model-from-provider @inferred @source:internal/proxy/anthropic_json.go#respondAnthropicJSON @questionable
  Scenario Outline: The message id and model are the ones the provider reported
    Given the client asked for model "claude-x"
    And the provider's reply carries id <provider_id> and model <provider_model>
    When a client sends POST /v1/messages without "stream": true
    Then "id" is <id>
    And "model" is <model>

    Examples:
      | provider_id  | provider_model | id           | model         |
      | "chatcmpl-1" | "gpt-4o-mini"  | "chatcmpl-1" | "gpt-4o-mini" |
      | (none)       | (none)         | "msg_chicco" | ""            |

  @id:api-gateway.anthropic-messages.004 @rule:api-gateway.anthropic-messages.empty-reply-still-succeeds @inferred @source:internal/proxy/anthropic_json.go#respondAnthropicJSON @questionable
  Scenario: A provider reply with no readable content still comes back as an empty successful message
    Given the provider accepted the request but its reply contains no readable content
    When a client sends POST /v1/messages without "stream": true
    Then the response status is 200
    And "content" is an empty list
    And "stop_reason" is "end_turn"
    And "id" is "msg_chicco"
