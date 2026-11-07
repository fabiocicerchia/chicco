@domain:cli-providers @area:cli-response-synthesis @from:proxy/clisynth
Feature: Answering in OpenAI shape from a local CLI tool
  Lets callers use a local CLI tool as a provider without noticing, by turning the tool's plain text into the OpenAI chat completion reply or stream they asked for.

  Background:
    Given a CLI provider "local-cli" serving the model "m1"
    And the CLI tool answers "hello from m1"

  @id:cli-providers.cli-response-synthesis.004 @rule:cli-providers.response-synthesis.non-streamed-reply-is-chat-completion @verified @source:internal/proxy/clisynth.go#synthJSON @test:internal/proxy/cli_test.go#TestRunCLIEndToEnd
  Scenario: A non-streamed request served by a CLI tool gets a chat completion object
    Given the caller's request either omits "stream" or sets it to false
    When the caller sends the request to /v1/chat/completions
    Then the response is 200 with Content-Type application/json
    And the body has object "chat.completion", an id, a created time and model "m1"
    And the only choice has index 0, an assistant message with content "hello from m1" and finish_reason "stop"
    And the body carries a usage block and no [DONE] marker

  @id:cli-providers.cli-response-synthesis.005 @rule:cli-providers.response-synthesis.streamed-reply-is-openai-sse @verified @source:internal/proxy/clisynth.go#synthSSE @test:internal/proxy/cli_test.go#TestSynthSSEParsesAsOpenAI @test:internal/proxy/cli_test.go#TestRunCLIEndToEnd
  Scenario: A streamed request served by a CLI tool gets a complete OpenAI event stream
    Given the caller's request sets "stream" to true
    When the caller sends the request to /v1/chat/completions
    Then the response is 200 with Content-Type text/event-stream
    And the first event is a "chat.completion.chunk" whose single delta holds the assistant's whole answer "hello from m1" with finish_reason "stop"
    And a second chunk with no choices carries the usage
    And the stream ends with "data: [DONE]"

  @id:cli-providers.cli-response-synthesis.006 @rule:cli-providers.response-synthesis.usage-totals-prompt-plus-completion @verified @source:internal/proxy/clisynth.go#synthUsage @test:internal/proxy/cli_test.go#TestSynthSSEParsesAsOpenAI
  Scenario: A CLI-served reply reports prompt, completion and total tokens
    Given the CLI tool's reply is counted as 4 prompt tokens and 12 completion tokens
    When the caller sends a chat completion request
    Then the usage shows prompt_tokens 4
    And the usage shows completion_tokens 12
    And the usage shows total_tokens 16

  @id:cli-providers.cli-response-synthesis.007 @rule:cli-providers.response-synthesis.reply-identifies-chicco-and-model @verified @source:internal/proxy/clisynth.go#synthID @test:internal/proxy/cli_test.go#TestSynthSSEParsesAsOpenAI
  Scenario: A CLI-served reply names the model that answered and carries a chicco response id
    Given the caller's request sets "stream" to true
    When the caller sends a chat completion request
    Then every chunk carries model "m1"
    And every chunk carries the same id
    And the id starts with "chatcmpl-chicco-"
    And two replies, even in the same second, have different ids
