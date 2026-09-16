import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:msa_admin/api_client.dart';

/// _client 用一个可编程的假传输构造客户端，并把收到的请求记进 captured。
ApiClient _client(
  List<http.BaseRequest> captured, {
  int status = 200,
  String body = '{}',
  bool throwOnSend = false,
}) {
  final mock = MockClient((request) async {
    captured.add(request);
    if (throwOnSend) throw http.ClientException('boom');
    return http.Response(body, status,
        headers: {'content-type': 'application/json'});
  });
  return ApiClient(
    baseUrl: 'http://agent.test',
    adminKey: 'admin-secret',
    httpClient: mock,
  );
}

void main() {
  group('认证头', () {
    test('管理面用 Bearer，不带 relay 的 X-Admin-Key', () async {
      final captured = <http.BaseRequest>[];
      final client = _client(captured, body: '{"status":"ok"}');
      await client.health();

      expect(captured.single.headers['Authorization'], 'Bearer admin-secret');
      // agent 与 relay 的管理面头不同；走错头会被后端 401。
      expect(captured.single.headers.containsKey('X-Admin-Key'), isFalse);
    });
  });

  group('查询参数', () {
    test('空筛选项不出现在 URL 上', () async {
      final captured = <http.BaseRequest>[];
      final client = _client(captured, body: '{"requests":[]}');
      await client.listRequests(outcome: 'normal');

      final uri = captured.single.url;
      expect(uri.queryParameters['outcome'], 'normal');
      expect(uri.queryParameters['limit'], '50');
      // 空串筛选会被后端当成有效过滤值，必须在客户端剔掉。
      expect(uri.queryParameters.containsKey('model_id'), isFalse);
      expect(uri.queryParameters.containsKey('cursor'), isFalse);
    });

    test('request_id 做百分号编码', () async {
      final captured = <http.BaseRequest>[];
      final client = _client(captured, body: '{"request_id":"a/b"}');
      await client.getRequest('a/b');

      expect(captured.single.url.path, '/admin/requests/a%2Fb');
    });

    test('baseUrl 末尾斜杠不会拼出双斜杠', () async {
      final captured = <http.BaseRequest>[];
      final mock = MockClient((request) async {
        captured.add(request);
        return http.Response('{}', 200);
      });
      final client = ApiClient(
          baseUrl: 'http://agent.test/', adminKey: 'k', httpClient: mock);
      await client.health();

      expect(captured.single.url.path, '/admin/health');
    });
  });

  group('错误分类', () {
    test('传输异常归为不可达并带上地址', () async {
      final client = _client([], throwOnSend: true);
      await expectLater(
        client.health(),
        throwsA(isA<UnreachableException>()
            .having((e) => e.baseUrl, 'baseUrl', 'http://agent.test')),
      );
    });

    test('401 归为密钥无效', () async {
      final client = _client([], status: 401, body: '{}');
      await expectLater(client.health(), throwsA(isA<UnauthorizedException>()));
    });

    test('错误信封里的 code 被保留供界面分支', () async {
      final client = _client([],
          status: 400,
          body: '{"error":{"code":"invalid_request","message":"bad window"}}');
      await expectLater(
        client.stats(window: 'nope'),
        throwsA(isA<ApiErrorException>()
            .having((e) => e.code, 'code', 'invalid_request')
            .having((e) => e.message, 'message', 'bad window')),
      );
    });

    test('非 JSON 错误体不致崩，退化成带状态码的提示', () async {
      final client = _client([], status: 502, body: '<html>bad gateway</html>');
      await expectLater(
        client.stats(),
        throwsA(isA<ApiErrorException>()
            .having((e) => e.code, 'code', 'unknown')
            .having((e) => e.status, 'status', 502)),
      );
    });
  });

  group('端点形状', () {
    test('outbox 重试是 POST 到 retry 子路径', () async {
      final captured = <http.BaseRequest>[];
      final client = _client(captured);
      await client.retryOutbox('req-1:0');

      expect(captured.single.method, 'POST');
      expect(captured.single.url.path, '/admin/outbox/req-1%3A0/retry');
    });

    test('live 解出 degraded 标记', () async {
      final client = _client([], body: '{"entries":[],"degraded":true}');
      final page = await client.live();

      // 空列表 + degraded 表示「看不到」，界面靠这个标记区分它与「没流量」。
      expect(page.entries, isEmpty);
      expect(page.degraded, isTrue);
    });
  });
}
