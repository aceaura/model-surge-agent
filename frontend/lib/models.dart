/// 管理面 DTO，镜像后端的 contract/agentv1。字段名必须逐字一致。
library;

class Health {
  const Health({
    required this.status,
    required this.database,
    required this.cache,
    required this.relay,
    required this.outboxPending,
    required this.outboxDead,
  });

  final String status;
  final String database;
  final String cache;
  final String relay;
  final int outboxPending;
  final int outboxDead;

  bool get ok => status == 'ok';

  /// cacheDisabled 与 cache down 要分开显示：前者是部署选择，不是故障。
  bool get cacheDisabled => cache == 'disabled';

  static Health fromJson(Map<String, dynamic> j) => Health(
        status: j['status'] as String? ?? 'unknown',
        database: j['database'] as String? ?? 'unknown',
        cache: j['cache'] as String? ?? 'unknown',
        relay: j['relay'] as String? ?? 'unknown',
        outboxPending: j['outbox_pending'] as int? ?? 0,
        outboxDead: j['outbox_dead'] as int? ?? 0,
      );
}

class RequestSummary {
  const RequestSummary({
    required this.requestId,
    required this.at,
    required this.inboundProtocol,
    required this.outboundProtocol,
    required this.userModel,
    required this.modelId,
    required this.account,
    required this.outcome,
    required this.statusCode,
    required this.attempts,
    required this.triedIds,
    required this.committed,
    required this.stream,
    required this.usageEstimated,
    required this.inputTokens,
    required this.outputTokens,
    required this.cacheReadTokens,
    required this.latencyMs,
    required this.firstTokenMs,
    required this.errorCode,
    required this.errorMessage,
  });

  final String requestId;
  final DateTime? at;
  final String inboundProtocol;
  final String outboundProtocol;
  final String userModel;
  final String modelId;
  final String account;
  final String outcome;
  final int statusCode;
  final int attempts;
  final List<String> triedIds;
  final bool committed;
  final bool stream;
  final bool usageEstimated;
  final int inputTokens;
  final int outputTokens;
  final int cacheReadTokens;
  final int latencyMs;
  final int firstTokenMs;
  final String errorCode;
  final String errorMessage;

  static RequestSummary fromJson(Map<String, dynamic> j) => RequestSummary(
        requestId: j['request_id'] as String? ?? '',
        at: _time(j['at']),
        inboundProtocol: j['inbound_protocol'] as String? ?? '',
        outboundProtocol: j['outbound_protocol'] as String? ?? '',
        userModel: j['user_model'] as String? ?? '',
        modelId: j['model_id'] as String? ?? '',
        account: j['account'] as String? ?? '',
        outcome: j['outcome'] as String? ?? '',
        statusCode: j['status_code'] as int? ?? 0,
        attempts: j['attempts'] as int? ?? 0,
        triedIds: (j['tried_ids'] as List<dynamic>? ?? const [])
            .map((e) => e as String)
            .toList(),
        committed: j['committed'] as bool? ?? false,
        stream: j['stream'] as bool? ?? false,
        usageEstimated: j['usage_estimated'] as bool? ?? false,
        inputTokens: j['input_tokens'] as int? ?? 0,
        outputTokens: j['output_tokens'] as int? ?? 0,
        cacheReadTokens: j['cache_read_tokens'] as int? ?? 0,
        latencyMs: j['latency_ms'] as int? ?? 0,
        firstTokenMs: j['first_token_ms'] as int? ?? 0,
        errorCode: j['error_code'] as String? ?? '',
        errorMessage: j['error_message'] as String? ?? '',
      );
}

class RequestPage {
  const RequestPage({required this.requests, required this.nextCursor});

  final List<RequestSummary> requests;

  /// nextCursor 是后端给的不透明串，原样回传。空表示没有下一页。
  final String nextCursor;

  bool get hasMore => nextCursor.isNotEmpty;

  static RequestPage fromJson(Map<String, dynamic> j) => RequestPage(
        requests: (j['requests'] as List<dynamic>? ?? const [])
            .map((e) => RequestSummary.fromJson(e as Map<String, dynamic>))
            .toList(),
        nextCursor: j['next_cursor'] as String? ?? '',
      );
}

class LiveEntry {
  const LiveEntry({
    required this.requestId,
    required this.at,
    required this.inboundProtocol,
    required this.outboundProtocol,
    required this.userModel,
    required this.modelId,
    required this.account,
    required this.outcome,
    required this.statusCode,
    required this.attempts,
    required this.stream,
    required this.latencyMs,
    required this.firstTokenMs,
    required this.inputTokens,
    required this.outputTokens,
    required this.errorCode,
  });

  final String requestId;
  final DateTime? at;
  final String inboundProtocol;
  final String outboundProtocol;
  final String userModel;
  final String modelId;
  final String account;
  final String outcome;
  final int statusCode;
  final int attempts;
  final bool stream;
  final int latencyMs;
  final int firstTokenMs;
  final int inputTokens;
  final int outputTokens;
  final String errorCode;

  static LiveEntry fromJson(Map<String, dynamic> j) => LiveEntry(
        requestId: j['request_id'] as String? ?? '',
        at: _time(j['at']),
        inboundProtocol: j['inbound_protocol'] as String? ?? '',
        outboundProtocol: j['outbound_protocol'] as String? ?? '',
        userModel: j['user_model'] as String? ?? '',
        modelId: j['model_id'] as String? ?? '',
        account: j['account'] as String? ?? '',
        outcome: j['outcome'] as String? ?? '',
        statusCode: j['status_code'] as int? ?? 0,
        attempts: j['attempts'] as int? ?? 0,
        stream: j['stream'] as bool? ?? false,
        latencyMs: j['latency_ms'] as int? ?? 0,
        firstTokenMs: j['first_token_ms'] as int? ?? 0,
        inputTokens: j['input_tokens'] as int? ?? 0,
        outputTokens: j['output_tokens'] as int? ?? 0,
        errorCode: j['error_code'] as String? ?? '',
      );
}

class LivePage {
  const LivePage({required this.entries, required this.degraded});

  final List<LiveEntry> entries;

  /// degraded 为真表示缓存不可用，空列表是「看不到」而不是「没流量」。
  /// 两者在界面上必须区分，否则会把故障误读成空闲。
  final bool degraded;

  static const empty = LivePage(entries: [], degraded: false);

  static LivePage fromJson(Map<String, dynamic> j) => LivePage(
        entries: (j['entries'] as List<dynamic>? ?? const [])
            .map((e) => LiveEntry.fromJson(e as Map<String, dynamic>))
            .toList(),
        degraded: j['degraded'] as bool? ?? false,
      );
}

class StatBucket {
  const StatBucket({
    required this.minute,
    required this.total,
    required this.outcomes,
    required this.inputTokens,
    required this.outputTokens,
    required this.avgLatencyMs,
  });

  final DateTime? minute;
  final int total;
  final Map<String, int> outcomes;
  final int inputTokens;
  final int outputTokens;
  final int avgLatencyMs;

  static StatBucket fromJson(Map<String, dynamic> j) => StatBucket(
        minute: _time(j['minute']),
        total: j['total'] as int? ?? 0,
        outcomes: _counts(j['outcomes']),
        inputTokens: j['input_tokens'] as int? ?? 0,
        outputTokens: j['output_tokens'] as int? ?? 0,
        avgLatencyMs: j['avg_latency_ms'] as int? ?? 0,
      );
}

class StatTotals {
  const StatTotals({
    required this.total,
    required this.outcomes,
    required this.inputTokens,
    required this.outputTokens,
    required this.successRate,
    required this.qps,
  });

  final int total;
  final Map<String, int> outcomes;
  final int inputTokens;
  final int outputTokens;
  final double successRate;
  final double qps;

  static const empty = StatTotals(
      total: 0, outcomes: {}, inputTokens: 0, outputTokens: 0,
      successRate: 0, qps: 0);

  static StatTotals fromJson(Map<String, dynamic> j) => StatTotals(
        total: j['total'] as int? ?? 0,
        outcomes: _counts(j['outcomes']),
        inputTokens: j['input_tokens'] as int? ?? 0,
        outputTokens: j['output_tokens'] as int? ?? 0,
        successRate: (j['success_rate'] as num? ?? 0).toDouble(),
        qps: (j['qps'] as num? ?? 0).toDouble(),
      );
}

class Stats {
  const Stats({
    required this.window,
    required this.buckets,
    required this.totals,
    required this.degraded,
  });

  final String window;
  final List<StatBucket> buckets;
  final StatTotals totals;
  final bool degraded;

  static const empty = Stats(
      window: '1h', buckets: [], totals: StatTotals.empty, degraded: false);

  static Stats fromJson(Map<String, dynamic> j) => Stats(
        window: j['window'] as String? ?? '',
        buckets: (j['buckets'] as List<dynamic>? ?? const [])
            .map((e) => StatBucket.fromJson(e as Map<String, dynamic>))
            .toList(),
        totals: StatTotals.fromJson(
            j['totals'] as Map<String, dynamic>? ?? const {}),
        degraded: j['degraded'] as bool? ?? false,
      );
}

class OutboxEntry {
  const OutboxEntry({
    required this.reportId,
    required this.requestId,
    required this.modelId,
    required this.outcome,
    required this.attempts,
    required this.nextAttemptAt,
    required this.lastError,
    required this.createdAt,
  });

  final String reportId;
  final String requestId;
  final String modelId;
  final String outcome;
  final int attempts;
  final DateTime? nextAttemptAt;
  final String lastError;
  final DateTime? createdAt;

  static OutboxEntry fromJson(Map<String, dynamic> j) => OutboxEntry(
        reportId: j['report_id'] as String? ?? '',
        requestId: j['request_id'] as String? ?? '',
        modelId: j['model_id'] as String? ?? '',
        outcome: j['outcome'] as String? ?? '',
        attempts: j['attempts'] as int? ?? 0,
        nextAttemptAt: _time(j['next_attempt_at']),
        lastError: j['last_error'] as String? ?? '',
        createdAt: _time(j['created_at']),
      );
}

class ModelInfo {
  const ModelInfo({
    required this.name,
    required this.collection,
    required this.policy,
    required this.protocol,
    required this.enabled,
    required this.outboundReady,
  });

  final String name;
  final String collection;
  final String policy;
  final String protocol;
  final bool enabled;

  /// outboundReady 为假意味着调度层可能把请求路由到本服务没实现出站编解码
  /// 的协议上，那会以 invalid_model 收场。
  final bool outboundReady;

  static ModelInfo fromJson(Map<String, dynamic> j) => ModelInfo(
        name: j['name'] as String? ?? '',
        collection: j['collection'] as String? ?? '',
        policy: j['policy'] as String? ?? '',
        protocol: j['protocol'] as String? ?? '',
        enabled: j['enabled'] as bool? ?? false,
        outboundReady: j['outbound_ready'] as bool? ?? false,
      );
}

class ModelsPage {
  const ModelsPage({
    required this.models,
    required this.inbound,
    required this.outbound,
    required this.cached,
  });

  final List<ModelInfo> models;
  final List<String> inbound;
  final List<String> outbound;
  final bool cached;

  static ModelsPage fromJson(Map<String, dynamic> j) => ModelsPage(
        models: (j['models'] as List<dynamic>? ?? const [])
            .map((e) => ModelInfo.fromJson(e as Map<String, dynamic>))
            .toList(),
        inbound: _strings(j['inbound']),
        outbound: _strings(j['outbound']),
        cached: j['cached'] as bool? ?? false,
      );
}

/// outcome 语义由后端的 relayclient 定义，界面按它上色。
const outcomeNormal = 'normal';
const outcomeAbnormal = 'abnormal';
const outcomeRetrying = 'retrying';
const outcomeInvalidModel = 'invalid_model';
const outcomeContextExceeded = 'context_exceeded';

const allOutcomes = [
  outcomeNormal,
  outcomeAbnormal,
  outcomeRetrying,
  outcomeInvalidModel,
  outcomeContextExceeded,
];

DateTime? _time(Object? raw) {
  if (raw is! String || raw.isEmpty) return null;
  return DateTime.tryParse(raw)?.toLocal();
}

Map<String, int> _counts(Object? raw) {
  if (raw is! Map) return const {};
  return {
    for (final e in raw.entries) e.key as String: (e.value as num).toInt(),
  };
}

List<String> _strings(Object? raw) {
  if (raw is! List) return const [];
  return raw.map((e) => e as String).toList();
}
