/// 客户端与 agent 管理面的唯一 HTTP 出口。
///
/// 三类失败分开建模，让界面能给出可操作提示而不是通用文案：
/// 服务不可达 / 管理密钥无效 / 服务端校验错误。
/// 本文件不打印任何日志——管理密钥会出现在请求头上。
library;

import 'dart:convert';

import 'package:http/http.dart' as http;

import 'models.dart';

sealed class ApiException implements Exception {
  const ApiException(this.message);
  final String message;
  @override
  String toString() => message;
}

/// 服务不可达。携带所用地址，便于运维者核对配置。
class UnreachableException extends ApiException {
  const UnreachableException(this.baseUrl, super.message);
  final String baseUrl;
}

class UnauthorizedException extends ApiException {
  const UnauthorizedException() : super('管理密钥无效');
}

/// 服务端返回的错误信封。code 供界面分支，界面不解析 message 文本。
class ApiErrorException extends ApiException {
  const ApiErrorException(this.code, super.message, this.status);
  final String code;
  final int status;
}

class ApiClient {
  ApiClient({
    required this.baseUrl,
    required this.adminKey,
    http.Client? httpClient,
  }) : _http = httpClient ?? http.Client();

  final String baseUrl;
  final String adminKey;
  final http.Client _http;

  static const _timeout = Duration(seconds: 15);

  /// 管理面用 Bearer。数据面不做客户端鉴权，两者不共用任何头。
  Map<String, String> get _headers => {'Authorization': 'Bearer $adminKey'};

  Uri _uri(String path, [Map<String, String> query = const {}]) {
    final root = baseUrl.endsWith('/')
        ? baseUrl.substring(0, baseUrl.length - 1)
        : baseUrl;
    final trimmed = {
      for (final e in query.entries)
        if (e.value.isNotEmpty) e.key: e.value,
    };
    return Uri.parse('$root$path').replace(
        queryParameters: trimmed.isEmpty ? null : trimmed);
  }

  Future<Map<String, dynamic>> _send(String method, Uri uri) async {
    http.Response response;
    try {
      final request = http.Request(method, uri)..headers.addAll(_headers);
      final streamed = await _http.send(request).timeout(_timeout);
      response = await http.Response.fromStream(streamed);
    } catch (_) {
      throw UnreachableException(baseUrl, '无法连接服务：$baseUrl');
    }

    if (response.statusCode == 401) {
      throw const UnauthorizedException();
    }
    if (response.statusCode >= 200 && response.statusCode < 300) {
      if (response.body.isEmpty) return const {};
      return jsonDecode(response.body) as Map<String, dynamic>;
    }
    throw _errorOf(response);
  }

  ApiErrorException _errorOf(http.Response response) {
    try {
      final decoded = jsonDecode(response.body) as Map<String, dynamic>;
      final error = decoded['error'] as Map<String, dynamic>;
      return ApiErrorException(
        error['code'] as String? ?? 'unknown',
        error['message'] as String? ?? '请求失败',
        response.statusCode,
      );
    } catch (_) {
      return ApiErrorException(
        'unknown',
        '请求失败（HTTP ${response.statusCode}）',
        response.statusCode,
      );
    }
  }

  /// health 永远返回 200，降级体现在 status 字段上。这样界面能区分
  /// 「服务降级」与「管理面自己不通」——后者会抛异常。
  Future<Health> health() async =>
      Health.fromJson(await _send('GET', _uri('/admin/health')));

  Future<RequestPage> listRequests({
    int limit = 50,
    String cursor = '',
    String outcome = '',
    String modelId = '',
    String userModel = '',
  }) async {
    final body = await _send(
      'GET',
      _uri('/admin/requests', {
        'limit': '$limit',
        'cursor': cursor,
        'outcome': outcome,
        'model_id': modelId,
        'user_model': userModel,
      }),
    );
    return RequestPage.fromJson(body);
  }

  Future<RequestSummary> getRequest(String requestId) async {
    final body = await _send(
        'GET', _uri('/admin/requests/${Uri.encodeComponent(requestId)}'));
    return RequestSummary.fromJson(body);
  }

  Future<LivePage> live({int limit = 100}) async =>
      LivePage.fromJson(await _send('GET', _uri('/admin/live', {'limit': '$limit'})));

  Future<Stats> stats({String window = '1h'}) async =>
      Stats.fromJson(await _send('GET', _uri('/admin/stats', {'window': window})));

  Future<List<OutboxEntry>> listOutbox({required String state}) async {
    final body = await _send('GET', _uri('/admin/outbox', {'state': state}));
    return (body['entries'] as List<dynamic>? ?? const [])
        .map((e) => OutboxEntry.fromJson(e as Map<String, dynamic>))
        .toList();
  }

  /// retryOutbox 重置死信的下次尝试时间，由后台 worker 接手重放。
  Future<void> retryOutbox(String reportId) => _send('POST',
      _uri('/admin/outbox/${Uri.encodeComponent(reportId)}/retry'));

  Future<ModelsPage> listModels() async =>
      ModelsPage.fromJson(await _send('GET', _uri('/admin/models')));

  void close() => _http.close();
}
