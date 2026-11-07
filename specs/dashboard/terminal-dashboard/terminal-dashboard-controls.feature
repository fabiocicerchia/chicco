@domain:dashboard @area:terminal-dashboard @from:proxy/ui
Feature: Driving the terminal dashboard
  Lets an operator running chicco in dashboard mode watch providers and logs side by side, move around them from the keyboard, start a model test and quit, without leaving the terminal.

  Background:
    Given chicco is running with its terminal dashboard in the foreground

  @id:dashboard.terminal-dashboard.001 @rule:dashboard.terminal.refresh-every-second @inferred @source:internal/proxy/ui.go#tick
  Scenario: The dashboard refreshes provider and log state once a second
    Given provider "groq" has just served a request
    When one second passes
    Then the dashboard shows groq's updated usage and the new log lines without any key being pressed
