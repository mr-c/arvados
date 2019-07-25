# Copyright (C) The Arvados Authors. All rights reserved.
#
# SPDX-License-Identifier: AGPL-3.0

require 'test_helper'

class HealthcheckControllerTest < ActionController::TestCase
  reset_api_fixtures :after_each_test, false
  reset_api_fixtures :after_suite, false

  [
    [false, nil, 404, 'disabled'],
    [true, nil, 401, 'authorization required'],
    [true, 'badformatwithnoBearer', 403, 'authorization error'],
    [true, 'Bearer wrongtoken', 403, 'authorization error'],
    [true, 'Bearer configuredmanagementtoken', 200, '{"health":"OK"}'],
  ].each do |enabled, header, error_code, error_msg|
    test "ping when #{if enabled then 'enabled' else 'disabled' end} with header '#{header}'" do
      if enabled
        Rails.configuration.ManagementToken = 'configuredmanagementtoken'
      else
        Rails.configuration.ManagementToken = ""
      end

      @request.headers['Authorization'] = header
      get :ping
      assert_response error_code

      resp = JSON.parse(@response.body)
      if error_code == 200
        assert_equal(JSON.load('{"health":"OK"}'), resp)
      else
        assert_equal(resp['errors'], error_msg)
      end
    end
  end
end
