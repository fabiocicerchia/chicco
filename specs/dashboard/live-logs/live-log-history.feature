@domain:dashboard @area:live-logs @from:proxy/logbuffer
Feature: Showing recent log lines
  Gives operators the proxy's recent log history in the terminal dashboard and the web dashboard's /v1/status feed, so they can see what chicco just did without tailing a file.

  Background:
    Given chicco is running

  @id:dashboard.live-logs.001 @rule:dashboard.live-logs.status-returns-recent-lines @inferred @source:internal/proxy/proxy.go#handleStatus
  Scenario Outline: The status feed carries the most recent log lines, oldest first
    Given chicco has logged <logged> complete lines since it started
    When the web dashboard requests GET /v1/status
    Then the response is 200 with a "logs" array of <returned> lines
    And the lines are the newest ones, oldest first
    And the "logs" field is <shape>

    Examples:
      | logged | returned | shape                      |
      | 0      | 0        | an empty array, never null |
      | 40     | 40       | a JSON array               |
      | 350    | 100      | a JSON array               |

  @id:dashboard.live-logs.002 @rule:dashboard.live-logs.history-capped-at-500 @verified @source:internal/proxy/run.go#Run @test:internal/proxy/ui_test.go#TestLogBufferRingAndSplit
  Scenario: Only the latest 500 log lines are kept
    Given chicco has logged 650 complete lines since it started
    When the operator scrolls the terminal dashboard's log pane all the way back
    Then the oldest line shown is line 151
    And lines 1 to 150 can no longer be shown anywhere

  @id:dashboard.live-logs.003 @rule:dashboard.live-logs.captured-when-headless @inferred @source:internal/proxy/run.go#serveHeadless
  Scenario: Headless chicco still feeds its logs to the web dashboard
    Given chicco was started headless, or with stdout not a terminal
    When chicco logs a line
    Then the line is printed on stderr
    And the line appears in the "logs" array of GET /v1/status
