@domain:provider-health @area:health-probes @from:proxy/health
Feature: Probing provider health
  Lets the operator see, before any real request is routed, which providers have a working key or login and which are unreachable, and has that state recover on its own.

  Background:
    Given chicco is running with a pool of active providers

  @id:provider-health.health-probes.001 @rule:provider-health.probes.http-reply-sets-state @verified @source:internal/proxy/health.go#probeProvider @test:internal/proxy/health_test.go#TestProbeProvider @test:internal/proxy/health_test.go#TestCheckHealthSetsSnapshot
  Scenario Outline: An HTTP provider's state follows how it answers a models listing made with its key
    Given an HTTP provider "acme" whose models listing answers <reply> to its configured key
    When chicco probes the providers
    Then provider "acme" is reported as "<state>" under providers in /health

    Examples:
      | reply         | state |
      | HTTP 200      | ok    |
      | HTTP 404      | ok    |
      | HTTP 401      | auth  |
      | HTTP 403      | auth  |
      | HTTP 503      | down  |
      | no connection | down  |

  @id:provider-health.health-probes.002 @rule:provider-health.probes.cli-login-state @verified @source:internal/proxy/health.go#probeCLI @test:internal/proxy/cli_test.go#TestProbeCLI
  Scenario Outline: A local CLI provider's state reflects whether its tool is installed and logged in
    Given a CLI provider "claude-cli" where <situation>
    When chicco probes the providers
    Then provider "claude-cli" is reported as "<state>" under providers in /health

    Examples:
      | situation                                                   | state |
      | its command is not installed on this machine                | down  |
      | its health_command exits with an error                      | auth  |
      | its health_command output lacks the health_expect text      | auth  |
      | its health_command output contains the health_expect text   | ok    |
      | it has no health_command and its credential file is missing | auth  |
      | it has no health_command and no credential file configured  | ok    |
