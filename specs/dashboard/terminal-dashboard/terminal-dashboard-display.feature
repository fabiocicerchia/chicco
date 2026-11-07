@domain:dashboard @area:terminal-dashboard @from:proxy/uirender
Feature: Reading the terminal dashboard
  Lets the operator running chicco see at a glance today's traffic, how close each provider is to its quota, when a limit lifts, and which recent log lines are failures.

  Background:
    Given the operator runs chicco with the terminal dashboard open

  @id:dashboard.terminal-dashboard.002 @rule:dashboard.terminal.usage-bar-colour-by-quota-share @verified @source:internal/proxy/uirender.go#renderBar @test:internal/proxy/ui_test.go#TestRenderBarColorAndFill
  Scenario Outline: A provider's usage bar turns amber and then red as its quota fills
    Given a provider has used <share> of its quota window
    When the dashboard refreshes
    Then the provider's usage bar is <colour>
    And the bar is <fill>

    Examples:
      | share | colour | fill                            |
      | 30%   | green  | about 30% filled                |
      | 60%   | amber  | about 60% filled                |
      | 84%   | amber  | about 84% filled                |
      | 85%   | red    | about 85% filled                |
      | 130%  | red    | completely filled and no longer |

  @id:dashboard.terminal-dashboard.003 @rule:dashboard.terminal.limit-reset-time-format @inferred @source:internal/proxy/uirender.go#fmtReset @questionable
  Scenario Outline: A rate-limited provider shows when its limit lifts
    Given it is Nov 7 at 10:00
    And a provider hit its limit and its window reopens in <remaining>
    When the dashboard refreshes
    Then the provider's row reads "limit · resets <shown>"

    Examples:
      | remaining           | shown       |
      | 45 minutes          | 10:45       |
      | 11 hours 59 minutes | 21:59       |
      | 14 hours            | Nov 8 00:00 |

  @id:dashboard.terminal-dashboard.004 @rule:dashboard.terminal.failure-log-lines-highlighted @verified @source:internal/proxy/uirender.go#isErrorLine @test:internal/proxy/ui_test.go#TestIsErrorLine
  Scenario Outline: Failure lines in the log pane stand out in red
    Given the log contains the line "<line>"
    When the dashboard refreshes
    Then that line is shown in <colour>

    Examples:
      | line                                                               | colour   |
      | chicco: groq — auth failed (invalid or missing API key)            | red      |
      | chicco: groq — unreachable at boot                                 | red      |
      | chicco: reload rejected — providers[0]: missing name               | red      |
      | chicco: groq (llama-3.3-70b) HTTP 429, blocked 1m0s                | red      |
      | chicco test: ✗ groq/llama-3.3-70b — limit — resets in 17h43m       | red      |
      | chicco: groq — healthy                                             | dim grey |
      | chicco test: ✓ groq/llama-3.3-70b — 44 tok · 44/200.0k tok · daily | dim grey |

  @id:dashboard.terminal-dashboard.005 @rule:dashboard.terminal.legend-explains-status-and-keys @inferred @source:internal/proxy/uirender.go#legendLine
  Scenario: The dashboard explains its status dots and key shortcuts
    Given the dashboard is open
    When the operator looks at the bottom of the provider table
    Then the legend reads "● ready   ◐ cooldown / limit   ● bad key / down   ○ checking"
    And it lists the keys "tab focus · ↑↓/pgup/pgdn scroll · t test · q quit"
