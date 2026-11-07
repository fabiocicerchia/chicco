@domain:usage-and-cost @area:spend-tracking @from:proxy/cost
Feature: Pricing requests and reporting session spend
  Gives operators a running cost for the session, worked out from the per-model prices in chicco.yaml, where traffic that could not be priced is always shown as unpriced and never counted as free.

  Background:
    Given chicco is running from chicco.yaml

  @id:usage-and-cost.spend-tracking.001 @rule:usage.spend.currency-labelled-not-assumed @verified @source:internal/proxy/cost.go#newCostTracker @test:internal/proxy/cost_test.go#TestCurrencyIsLabelledNotAssumed
  Scenario Outline: Prices with no currency are labelled unspecified rather than assumed
    Given the operator's pricing block has <prices> and <currency_setting>
    When the operator reads /v1/status
    Then cost.currency is "<shown>"

    Examples:
      | prices                | currency_setting | shown       |
      | a price for model "m" | no currency      | unspecified |
      | no model prices       | no currency      |             |
      | a price for model "m" | currency "EUR"   | EUR         |
