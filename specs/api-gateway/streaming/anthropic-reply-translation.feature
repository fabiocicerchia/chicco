@domain:api-gateway @area:streaming @from:proxy/anthropic-stream
Feature: Translating provider replies into Anthropic messages
  Serves Anthropic-SDK callers who reach OpenAI-compatible providers through /v1/messages: they need Anthropic-shaped events, stop reasons, tool calls and token usage whichever provider answers.

  Background:
    Given a provider in the pool that answers in the OpenAI chat-completions stream format
    And a client that calls POST /v1/messages

  @id:api-gateway.streaming.008 @rule:anthropic.stream.event-sequence @verified @source:internal/proxy/anthropic_stream.go#translateOpenAIStream @test:internal/proxy/anthropic_test.go#TestMessagesStreaming
  Scenario: A streamed text reply arrives as the Anthropic event sequence
    Given the client asked for "stream": true
    And the provider replies with the text "hi" and finishes with reason "stop"
    When the client sends the message
    Then the response status is 200 with content type text/event-stream
    And the events arrive in order: message_start, content_block_start of type text, content_block_delta with text_delta "hi", content_block_stop, message_delta, message_stop
    And message_start carries the provider's reply id and model, or the id "msg_chicco" when the provider sent none

  @id:api-gateway.streaming.009 @rule:anthropic.stream.stop-reason-mapping @verified @source:internal/proxy/anthropic_stream.go#mapStopReason @test:internal/proxy/anthropic_test.go#TestMessagesNonStreaming @test:internal/proxy/anthropic_test.go#TestMessagesToolUse @test:internal/proxy/anthropic_test.go#TestMessagesStreaming @questionable
  Scenario Outline: The provider's finish reason is reported as the matching Anthropic stop reason
    Given the provider finishes its reply with reason <finish_reason>
    When the client sends the message
    Then the reply's stop_reason is <stop_reason>
    And the stop_sequence is null

    Examples:
      | finish_reason  | stop_reason   |
      | stop           | end_turn      |
      | length         | max_tokens    |
      | tool_calls     | tool_use      |
      | content_filter | stop_sequence |
      | (none sent)    | end_turn      |
