@domain:usage-and-cost @area:usage-persistence @from:proxy/state
Feature: Keeping usage and cooldowns across restarts
  Lets an operator restart or reboot chicco without losing token counters, rate-limit windows or active provider cooldowns, so a daily quota cannot be re-spent just by restarting.

  Background:
    Given chicco runs with the default state file "chicco-state.json"

  @id:usage-and-cost.usage-persistence.001 @rule:usage.persistence.counters-survive-restart @verified @source:internal/proxy/state.go#EnablePersistence @test:internal/proxy/state_test.go#TestPersistenceRoundTrip
  Scenario: Token usage and request counts survive a restart
    Given provider "groq" serving model "m" has used 2000 tokens over 2 requests
    And the counters have been saved to the state file
    When the operator restarts chicco with the same state file
    Then the dashboard shows provider "groq" with 2000 tokens used
    And the dashboard shows provider "groq" with 2 requests

  @id:usage-and-cost.usage-persistence.002 @rule:usage.persistence.active-cooldown-survives-restart @verified @source:internal/proxy/state.go#EnablePersistence @test:internal/proxy/state_test.go#TestLimitBlockPersists
  Scenario: A provider still cooling down stays benched after a restart
    Given provider "groq" hit its usage limit and is benched for 2 more hours with reason "limit"
    And the state has been saved
    When the operator restarts chicco with the same state file
    Then provider "groq" is still shown cooling down with kind "limit"
    And about 2 hours of cooldown remain

  @id:usage-and-cost.usage-persistence.003 @rule:usage.persistence.expired-cooldown-not-restored @inferred @source:internal/proxy/state.go#EnablePersistence
  Scenario: A cooldown that ended while chicco was down is not restored
    Given provider "groq" was saved as benched until 15:00
    And chicco was stopped before 15:00
    When the operator restarts chicco at 15:30
    Then provider "groq" is shown available with no cooldown

  @id:usage-and-cost.usage-persistence.004 @rule:usage.persistence.old-usage-dropped-on-load @inferred @source:internal/proxy/state.go#EnablePersistence
  Scenario: Usage older than 25 hours is forgotten on restart
    Given provider "groq" saved one request of 999 tokens 26 hours ago
    And and one request of 100 tokens 1 hour ago
    When the operator restarts chicco with the same state file
    Then provider "groq" carries only the 100-token request into its rate-limit windows

  @id:usage-and-cost.usage-persistence.005 @rule:usage.persistence.unreadable-state-starts-empty @inferred @source:internal/proxy/state.go#EnablePersistence
  Scenario Outline: A missing or unreadable state file starts chicco with zero usage
    Given the state file is <state>
    When the operator starts chicco
    Then chicco starts serving
    And every provider shows 0 tokens used and no cooldown

    Examples:
      | state               |
      | missing (first run) |
      | not valid JSON      |
      | unreadable          |

  @id:usage-and-cost.usage-persistence.006 @rule:usage.persistence.removed-provider-usage-dropped @inferred @source:internal/proxy/state.go#EnablePersistence @questionable
  Scenario: Saved usage for a provider no longer in the config is discarded
    Given the state file holds usage for provider "oldprov"
    And "oldprov" has been removed from chicco.yaml
    When the operator restarts chicco
    Then no usage is restored for "oldprov"
    And the next save no longer contains "oldprov" usage

  @id:usage-and-cost.usage-persistence.007 @rule:usage.persistence.state-file-owner-only @inferred @source:internal/proxy/state.go#Persist
  Scenario: The saved usage profile is readable only by the operator
    Given chicco has recorded usage for provider "groq"
    When chicco saves its state
    Then the state file has permissions 0600
    And an interrupted save leaves the previous state file intact
