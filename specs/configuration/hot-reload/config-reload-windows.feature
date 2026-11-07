@domain:configuration @area:hot-reload @from:proxy/reload-windows
Feature: Applying config changes on Windows
  Tells operators running chicco on Windows how chicco.yaml edits take effect, since live reload there is not available.

  Background:
    Given chicco is running on Windows with a chicco.yaml config

  @id:configuration.hot-reload.007 @rule:configuration.hot-reload.windows-needs-restart @inferred @source:internal/proxy/reload_windows.go#watchSIGHUP
  Scenario: On Windows a config edit takes effect only after a restart
    Given the operator has edited chicco.yaml, for example to add the provider "backup-openai"
    When the operator waits without restarting chicco
    Then chicco keeps serving with the provider set it started with
    And "backup-openai" receives no requests
    And no "config reloaded" message appears in the log
    And after the operator restarts chicco, "backup-openai" is part of the provider pool
