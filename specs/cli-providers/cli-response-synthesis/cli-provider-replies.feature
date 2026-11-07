@domain:cli-providers @area:cli-response-synthesis @from:proxy/cli
Feature: Answering callers from a CLI tool's output
  Gives callers served by a CLI provider the same OpenAI-shaped reply and usage figures they would get from an HTTP provider, so clients and cost accounting work unchanged.

  Background:
    Given a provider of kind "cli" serves model "m1"

  @id:cli-providers.cli-response-synthesis.001 @rule:cli.replies.shape-follows-stream-flag @verified @source:internal/proxy/cli.go#cliUpstream @test:internal/proxy/cli_test.go#TestRunCLIEndToEnd
  Scenario Outline: A CLI-served reply is streamed only when the caller asked for streaming
    Given the tool prints "hello from m1"
    When a caller sends a chat request to /v1/chat/completions with stream <stream>
    Then the response has Content-Type <content_type>
    And the body is <body> containing "hello from m1"

    Examples:
      | stream  | content_type      | body                                        |
      | omitted | application/json  | one chat.completion object                  |
      | false   | application/json  | one chat.completion object                  |
      | true    | text/event-stream | server-sent events ending with data: [DONE] |

  @id:cli-providers.cli-response-synthesis.002 @rule:cli.replies.answer-from-json-path @verified @source:internal/proxy/cli.go#extractCompletion @test:internal/proxy/cli_test.go#TestDotGetAndExtract @questionable
  Scenario Outline: For a JSON-printing tool, the answer is the field named by result_path
    Given the provider has output <output> and result_path "result"
    And the tool prints <printed>
    When a caller sends a chat request
    Then the reply's content is <answer>

    Examples:
      | output | printed                 | answer       |
      | json   | {"result":"done"}       | done         |
      | json   | plain answer (not JSON) | plain answer |
      | text   |   plain answer          | plain answer |

  @id:cli-providers.cli-response-synthesis.003 @rule:cli.replies.token-usage-reported-or-estimated @verified @source:internal/proxy/cli.go#runCLI @test:internal/proxy/cli_test.go#TestDotGetAndExtract @test:internal/proxy/cli_test.go#TestRunCLIEndToEnd
  Scenario Outline: A CLI-served reply always carries token usage, exact when the tool reports it
    Given the provider <paths>
    And the tool prints a 400-character prompt's answer of 40 characters with usage output_tokens 42 and input_tokens 7
    When a caller sends a chat request
    Then the reply reports <prompt_tokens> prompt tokens and <completion_tokens> completion tokens
    And the provider's usage counters grow by the same amount

    Examples:
      | paths                                                                             | prompt_tokens                      | completion_tokens                 |
      | sets tokens_path "usage.output_tokens" and input_tokens_path "usage.input_tokens" | 7                                  | 42                                |
      | sets neither path                                                                 | 100 (about 4 characters per token) | 10 (about 4 characters per token) |
