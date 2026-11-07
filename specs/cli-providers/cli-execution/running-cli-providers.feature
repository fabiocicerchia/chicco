@domain:cli-providers @area:cli-execution @from:proxy/cli
Feature: Serving requests from a local CLI tool
  Lets an operator put a locally installed AI CLI (claude, codex, kiro…) in the provider pool, so callers get its answers through the same endpoint and a broken tool never reaches them.

  Background:
    Given a provider of kind "cli" is configured with a command and templated args

  @id:cli-providers.cli-execution.001 @rule:cli.execution.request-templated-into-command @verified @source:internal/proxy/cli.go#runCLI @test:internal/proxy/cli_test.go#TestRunCLIEndToEnd @test:internal/proxy/cli_test.go#TestRunCLIViaAnthropicEndpointGetsThePrompt @test:internal/proxy/cli_test.go#TestSplitMessages @questionable
  Scenario Outline: The caller's request reaches the CLI tool through the configured placeholders
    Given the provider's args contain the placeholder <placeholder>
    When a caller sends a chat request for model "m1" with system prompt "be terse" and user message "ping"
    Then the tool is run with <placeholder> replaced by <value>

    Examples:
      | placeholder     | value                                                               |
      | {{model}}       | m1                                                                  |
      | {{system}}      | be terse                                                            |
      | {{user}}        | ping                                                                |
      | {{prompt}}      | be terse, a blank line, then ping                                   |
      | {{output_file}} | with output_file: true, the path of a fresh temporary file the tool writes its answer to (empty otherwise) |

  @id:cli-providers.cli-execution.002 @rule:cli.execution.failed-run-fails-over @verified @source:internal/proxy/cli.go#cliRun @test:internal/proxy/cli_test.go#TestRunCLIFailureFailsOver
  Scenario Outline: A CLI tool that fails is benched and the request moves to the next provider
    Given a second, working provider serves the same model
    And the CLI tool <failure>
    When a caller sends a chat request
    Then the caller gets the working provider's answer
    And the CLI provider is put in cooldown

    Examples:
      | failure                                             |
      | exits with a non-zero status                        |
      | runs longer than its timeout                        |
      | is not installed                                    |
      | exits cleanly but its output file is gone           |

  @id:cli-providers.cli-execution.003 @rule:cli.execution.reported-error-not-served @verified @source:internal/proxy/cli.go#extractCompletion @test:internal/proxy/cli_test.go#TestDotGetAndExtract
  Scenario Outline: An error the CLI tool reports in its JSON output is never served as the answer
    Given the provider has output "json", result_path "result" and error_path "is_error"
    And a second, working provider serves the same model
    And the tool exits cleanly printing is_error <flag> with result "Not logged in"
    When a caller sends a chat request
    Then the caller gets <outcome>

    Examples:
      | flag    | outcome                                                                |
      | true    | the working provider's answer, and the CLI provider is put in cooldown |
      | 1       | the working provider's answer, and the CLI provider is put in cooldown |
      | "yes"   | the working provider's answer, and the CLI provider is put in cooldown |
      | false   | "Not logged in" as the CLI provider's answer                           |
      | "false" | "Not logged in" as the CLI provider's answer                           |
      | 0       | "Not logged in" as the CLI provider's answer                           |

  @id:cli-providers.cli-execution.004 @rule:cli.execution.run-time-limit @inferred @source:internal/proxy/cli.go#cliRun
  Scenario Outline: A CLI run is cut off after its time limit
    Given the provider sets timeout_seconds to <setting>
    When a caller sends a chat request and the tool keeps running
    Then the tool is stopped after <limit> and the request fails over to the next provider

    Examples:
      | setting | limit       |
      | nothing | 120 seconds |
      | 30      | 30 seconds  |
