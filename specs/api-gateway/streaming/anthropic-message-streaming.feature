@domain:api-gateway @area:streaming @from:proxy/anthropic-sse
Feature: Streaming Anthropic messages
  Lets Anthropic SDK callers get a streamed /v1/messages reply as live Anthropic server-sent events, whichever OpenAI-compatible provider answers it.

  Background:
    Given a provider in the pool answers chat completions as an OpenAI-style event stream
    And a caller sends a /v1/messages request with "stream": true

  @id:api-gateway.streaming.001 @rule:messages.streaming.anthropic-event-sequence @verified @source:internal/proxy/anthropic_sse.go#respondAnthropicStream @test:internal/proxy/anthropic_test.go#TestMessagesStreaming
  Scenario: A streamed text reply arrives as the Anthropic event sequence
    Given the provider streams the text "hi" and then finishes with reason "stop"
    When the caller sends the streaming request
    Then the response status is 200 with Content-Type "text/event-stream" and Cache-Control "no-cache"
    And the events arrive in order: message_start, content_block_start, content_block_delta, content_block_stop, message_delta, message_stop
    And every event is framed as an "event:" line followed by one "data:" line of JSON and a blank line
    And the content_block_start opens a text block at index 0 with empty text
    And the content_block_delta carries a text_delta with text "hi"
    And the message_delta carries stop_reason "end_turn"

  @id:api-gateway.streaming.002 @rule:messages.streaming.tool-use-blocks @inferred @source:internal/proxy/anthropic_sse.go#sseSink
  Scenario: A streamed tool call arrives as a tool_use block with partial JSON input
    Given the provider streams a call to tool "get_weather" with id "call_1", sending its arguments in pieces
    When the caller sends the streaming request
    Then a content_block_start opens a tool_use block with id "call_1", name "get_weather" and an empty input object
    And each piece of the arguments arrives as a content_block_delta of type input_json_delta carrying that piece as partial_json
    And the tool_use block is closed with a content_block_stop at the same index
    And a second tool call opens a new block at the next index

  @id:api-gateway.streaming.003 @rule:messages.streaming.events-delivered-incrementally @inferred @source:internal/proxy/anthropic_sse.go#writeSSEEvent
  Scenario: Each streamed event reaches the caller as soon as it is produced
    Given the provider streams a reply in several pieces with pauses between them
    When the caller sends the streaming request
    Then the caller receives each event before the provider has finished the whole reply
    And the reply does not arrive in one lump at the end

  @id:api-gateway.streaming.004 @rule:messages.streaming.usage-reports-output-tokens-only @inferred @source:internal/proxy/anthropic_sse.go#sseSink @questionable
  Scenario: A streamed reply reports output tokens but never input tokens
    Given the provider reports 5 prompt tokens and 2 completion tokens for the reply
    When the caller sends the streaming request
    Then the message_start event reports usage input_tokens 0 and output_tokens 0
    And the message_delta event reports usage output_tokens 2
    And no event reports the 5 input tokens

  @id:api-gateway.streaming.005 @rule:messages.streaming.fallback-message-id @inferred @source:internal/proxy/anthropic_sse.go#sseSink @questionable
  Scenario: A streamed message without a provider id is labelled msg_chicco
    Given the provider's stream carries no message id
    When the caller sends the streaming request
    Then the message_start event carries message id "msg_chicco"
    And the message_start event carries type "message", role "assistant" and the provider's model name

  @id:api-gateway.streaming.006 @rule:messages.streaming.cut-off-upstream-looks-complete @inferred @source:internal/proxy/anthropic_sse.go#respondAnthropicStream @questionable
  Scenario: A provider stream that breaks off mid-reply still ends as a normal completion
    Given the provider streams part of a reply and then drops the connection without a finish reason
    When the caller sends the streaming request
    Then the response status stays 200
    And the open content block is closed with content_block_stop
    And a message_delta with stop_reason "end_turn" is sent, followed by message_stop
    And the caller receives no error event

  @id:api-gateway.streaming.007 @rule:messages.streaming-empty-upstream-omits-message-start @inferred @source:internal/proxy/anthropic_sse.go#sseSink @questionable
  Scenario: A provider stream with no content yields a stream without message_start
    Given the provider answers with only the end-of-stream marker "[DONE]"
    When the caller sends the streaming request
    Then the response status is 200
    And the caller receives only a message_delta with stop_reason "end_turn" and output_tokens 0, then message_stop
    And no message_start event is sent
