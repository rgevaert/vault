import { module, test } from 'qunit';
import { setupApplicationTest } from 'ember-qunit';
import sinon from 'sinon';
import { click, currentURL, visit, settled } from '@ember/test-helpers';
import { supportedAuthBackends } from 'vault/helpers/supported-auth-backends';
import authForm from '../pages/components/auth-form';
import jwtForm from '../pages/components/auth-jwt';
import { create } from 'ember-cli-page-object';
import apiStub from 'vault/tests/helpers/noop-all-api-requests';
import authPage from 'vault/tests/pages/auth';
import logout from 'vault/tests/pages/logout';
import consoleClass from 'vault/tests/pages/components/console/ui-panel';

const consoleComponent = create(consoleClass);
const component = create(authForm);
const jwtComponent = create(jwtForm);

module('Acceptance | auth', function (hooks) {
  setupApplicationTest(hooks);

  hooks.beforeEach(function () {
    this.clock = sinon.useFakeTimers({
      now: Date.now(),
      shouldAdvanceTime: true,
    });
    this.server = apiStub({ usePassthrough: true });
    return logout.visit();
  });

  hooks.afterEach(function () {
    this.clock.restore();
    this.server.shutdown();
    return logout.visit();
  });

  test('auth query params', async function (assert) {
    let backends = supportedAuthBackends();
    assert.expect(backends.length + 1);
    await visit('/vault/auth');
    assert.equal(currentURL(), '/vault/auth?with=token');
    for (let backend of backends.reverse()) {
      await component.selectMethod(backend.type);
      assert.equal(
        currentURL(),
        `/vault/auth?with=${backend.type}`,
        `has the correct URL for ${backend.type}`
      );
    }
  });

  test('it clears token when changing selected auth method', async function (assert) {
    await visit('/vault/auth');
    assert.equal(currentURL(), '/vault/auth?with=token');
    await component.token('token').selectMethod('github');
    await component.selectMethod('token');
    assert.equal(component.tokenValue, '', 'it clears the token value when toggling methods');
  });

  test('it sends the right attributes when authenticating', async function (assert) {
    let backends = supportedAuthBackends();
    await visit('/vault/auth');
    for (let backend of backends.reverse()) {
      await component.selectMethod(backend.type);
      if (backend.type === 'github') {
        await component.token('token');
      }
      if (backend.type === 'jwt' || backend.type === 'oidc') {
        await jwtComponent.role('test');
      }
      await component.login();
      let lastRequest = this.server.passthroughRequests[this.server.passthroughRequests.length - 1];
      let body = JSON.parse(lastRequest.requestBody);
      // Note: x-vault-token used to be lowercase prior to upgrade
      if (backend.type === 'token') {
        assert.ok(
          Object.keys(lastRequest.requestHeaders).includes('X-Vault-Token'),
          'token uses vault token header'
        );
      } else if (backend.type === 'github') {
        assert.ok(Object.keys(body).includes('token'), 'GitHub includes token');
      } else if (backend.type === 'jwt' || backend.type === 'oidc') {
        let authReq = this.server.passthroughRequests[this.server.passthroughRequests.length - 2];
        body = JSON.parse(authReq.requestBody);
        assert.ok(Object.keys(body).includes('role'), `${backend.type} includes role`);
      } else {
        assert.ok(Object.keys(body).includes('password'), `${backend.type} includes password`);
      }
    }
  });

  test('it shows the token warning beacon on the menu', async function (assert) {
    let authService = this.owner.lookup('service:auth');
    await authPage.login();
    await settled();
    await consoleComponent.runCommands([
      'write -field=client_token auth/token/create policies=default ttl=1h',
    ]);
    let token = consoleComponent.lastTextOutput;
    await logout.visit();
    await settled();
    await authPage.login(token);
    await settled();
    this.clock.tick(authService.IDLE_TIMEOUT);
    authService.shouldRenew();
    await settled();
    assert.dom('[data-test-allow-expiration]').exists('shows expiration beacon');

    await visit('/vault/access');

    assert.dom('[data-test-allow-expiration]').doesNotExist('hides beacon when the api is used again');
  });

  test('it shows the push notification warning only for okta auth method after submit', async function (assert) {
    await visit('/vault/auth');
    await component.selectMethod('token');
    await click('[data-test-auth-submit]');
    assert
      .dom('[data-test-auth-message="push"]')
      .doesNotExist('message is not shown for other authentication methods');

    await component.selectMethod('okta');
    await click('[data-test-auth-submit]');
    assert.dom('[data-test-auth-message="push"]').exists('shows push notification message');
  });
});
