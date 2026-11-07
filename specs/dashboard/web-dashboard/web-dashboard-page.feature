@domain:dashboard @area:web-dashboard @from:proxy/web
Feature: Opening the web dashboard
  Lets an operator watch the proxy's providers and logs from a browser, served by chicco itself with nothing else to deploy or fetch.

  Background:
    Given chicco is running

  @id:dashboard.web-dashboard.001 @rule:dashboard.web.page-served-self-contained @verified @source:internal/proxy/web.go#handleDashboard @test:internal/proxy/proxy_test.go#TestInboundAuth
  Scenario: The dashboard is served as one self-contained web page
    Given the operator is allowed to reach the dashboard
    When the operator opens /dashboard in a browser
    Then the response status is 200
    And the response is an HTML page with content type "text/html; charset=utf-8"
    And the page needs no other file from chicco and nothing from the internet to load

  @id:dashboard.web-dashboard.002 @rule:dashboard.web.only-get-allowed @inferred @source:internal/proxy/web.go#handleDashboard @questionable
  Scenario Outline: The dashboard page can only be fetched, not posted to
    Given the operator is allowed to reach the dashboard
    When a client sends a <method> request to /dashboard
    Then the response status is 405
    And the body reads "method not allowed"

    Examples:
      | method |
      | POST   |
      | PUT    |
      | DELETE |
