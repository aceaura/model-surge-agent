/// 总览页：窗口内的 QPS、成功率、outcome 分布与 token 曲线，外加健康状态。
library;

import 'dart:async';

import 'package:flutter/material.dart';

import '../api_client.dart';
import '../models.dart';
import '../theme.dart';
import '../ui/feedback.dart';
import '../ui/spark.dart';

const _windows = ['5m', '15m', '1h', '6h', '24h'];

class DashboardPage extends StatefulWidget {
  const DashboardPage({
    super.key,
    required this.client,
    required this.onOpenSettings,
  });

  final ApiClient client;
  final VoidCallback onOpenSettings;

  @override
  State<DashboardPage> createState() => _DashboardPageState();
}

class _DashboardPageState extends State<DashboardPage> {
  String _window = '1h';
  Stats _stats = Stats.empty;
  Health? _health;
  Object? _error;
  bool _loading = true;
  Timer? _timer;

  @override
  void initState() {
    super.initState();
    _load();
    _timer = Timer.periodic(const Duration(seconds: 10), (_) => _load());
  }

  @override
  void dispose() {
    _timer?.cancel();
    super.dispose();
  }

  Future<void> _load() async {
    try {
      final stats = await widget.client.stats(window: _window);
      final health = await widget.client.health();
      if (!mounted) return;
      setState(() {
        _stats = stats;
        _health = health;
        _error = null;
        _loading = false;
      });
    } catch (e) {
      if (!mounted) return;
      setState(() {
        _error = e;
        _loading = false;
      });
    }
  }

  void _pick(String window) {
    setState(() {
      _window = window;
      _loading = true;
    });
    _load();
  }

  @override
  Widget build(BuildContext context) {
    if (_error != null && _stats.totals.total == 0) {
      return ErrorPanel(
        error: _error!,
        onRetry: _load,
        onOpenSettings: widget.onOpenSettings,
      );
    }
    final t = context.tokens;
    return Column(
      children: [
        if (_stats.degraded) const DegradedBanner(message: '缓存不可用，统计为空不代表没有流量'),
        // 页头:大标题 + 弱色窗口标注 + 右侧窗口选择器(与姊妹仓页头同一版式)
        Padding(
          padding: const EdgeInsets.fromLTRB(24, 20, 24, 14),
          child: Row(
            children: [
              Text(
                '总览',
                style: TextStyle(
                  fontSize: 21,
                  fontWeight: FontWeight.w700,
                  color: t.ink,
                ),
              ),
              const SizedBox(width: 8),
              Text(
                '窗口 $_window',
                style: TextStyle(fontSize: 12.5, color: t.faint),
              ),
              const Spacer(),
              SegmentedButton<String>(
                segments: [
                  for (final w in _windows)
                    ButtonSegment(value: w, label: Text(w)),
                ],
                selected: {_window},
                onSelectionChanged: (s) => _pick(s.first),
              ),
              const SizedBox(width: 10),
              if (_loading)
                const SizedBox(
                  width: 16,
                  height: 16,
                  child: CircularProgressIndicator(strokeWidth: 2),
                ),
            ],
          ),
        ),
        Expanded(
          child: ListView(
            padding: const EdgeInsets.fromLTRB(24, 0, 24, 24),
            children: [
              _healthCard(context),
              const SizedBox(height: 12),
              _totalsRow(context),
              const SizedBox(height: 12),
              _outcomesCard(context),
              const SizedBox(height: 12),
              _trendCard(context, '请求量（每分钟）', [
                for (final b in _stats.buckets) b.total,
              ], t.primary),
              const SizedBox(height: 12),
              _trendCard(context, '输出 token（每分钟）', [
                for (final b in _stats.buckets) b.outputTokens,
              ], t.violet),
              const SizedBox(height: 12),
              _trendCard(context, '平均延迟 ms（每分钟）', [
                for (final b in _stats.buckets) b.avgLatencyMs,
              ], t.warn),
            ],
          ),
        ),
      ],
    );
  }

  Widget _healthCard(BuildContext context) {
    final health = _health;
    if (health == null) return const SizedBox.shrink();
    return Card(
      child: Padding(
        padding: const EdgeInsets.all(16),
        child: Wrap(
          spacing: 24,
          runSpacing: 12,
          children: [
            _dot(context, '总体', health.status),
            _dot(context, 'database', health.database),
            _dot(context, 'cache', health.cache),
            _dot(context, 'relay', health.relay),
            _pair('outbox 待发', '${health.outboxPending}'),
            _pair('outbox 死信', '${health.outboxDead}'),
          ],
        ),
      ),
    );
  }

  Widget _dot(BuildContext context, String label, String state) {
    final t = context.tokens;
    // disabled 用中性色：它是部署选择而不是故障。
    final color = switch (state) {
      'ok' => t.success,
      'disabled' => t.faint,
      _ => t.danger,
    };
    return Row(
      mainAxisSize: MainAxisSize.min,
      children: [
        Icon(Icons.circle, size: 9, color: color),
        const SizedBox(width: 6),
        Text('$label $state', style: TextStyle(fontSize: 12.5, color: t.dim)),
      ],
    );
  }

  Widget _pair(String label, String value) {
    final t = context.tokens;
    return Row(
      mainAxisSize: MainAxisSize.min,
      children: [
        Text(label, style: TextStyle(fontSize: 12.5, color: t.faint)),
        const SizedBox(width: 6),
        Text(
          value,
          style: TextStyle(
            fontSize: 12.5,
            fontWeight: FontWeight.w600,
            color: t.ink,
          ),
        ),
      ],
    );
  }

  Widget _totalsRow(BuildContext context) {
    final t = _stats.totals;
    final metrics = <(String, String)>[
      ('请求数', '${t.total}'),
      ('QPS', t.qps.toStringAsFixed(3)),
      (
        '成功率',
        t.total == 0 ? '-' : '${(t.successRate * 100).toStringAsFixed(1)}%',
      ),
      ('输入 token', compact(t.inputTokens)),
      ('输出 token', compact(t.outputTokens)),
    ];
    return Row(
      children: [
        for (var i = 0; i < metrics.length; i++) ...[
          if (i > 0) const SizedBox(width: 12),
          Expanded(child: _metric(context, metrics[i].$1, metrics[i].$2)),
        ],
      ],
    );
  }

  Widget _metric(BuildContext context, String label, String value) {
    final t = context.tokens;
    return Card(
      child: Padding(
        padding: const EdgeInsets.symmetric(vertical: 14, horizontal: 16),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text(label, style: TextStyle(fontSize: 12, color: t.faint)),
            const SizedBox(height: 6),
            Text(
              value,
              style: TextStyle(
                fontSize: 20,
                fontWeight: FontWeight.w700,
                color: t.ink,
              ),
            ),
          ],
        ),
      ),
    );
  }

  Widget _outcomesCard(BuildContext context) {
    final t = context.tokens;
    final counts = _stats.totals.outcomes;
    return Card(
      child: Padding(
        padding: const EdgeInsets.all(16),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text(
              'outcome 分布',
              style: TextStyle(
                fontSize: 13.5,
                fontWeight: FontWeight.w600,
                color: t.ink,
              ),
            ),
            const SizedBox(height: 12),
            Wrap(
              spacing: 14,
              runSpacing: 8,
              children: [
                for (final o in allOutcomes)
                  Row(
                    mainAxisSize: MainAxisSize.min,
                    children: [
                      OutcomeChip(outcome: o),
                      const SizedBox(width: 6),
                      Text(
                        '${counts[o] ?? 0}',
                        style: TextStyle(fontSize: 12.5, color: t.dim),
                      ),
                    ],
                  ),
              ],
            ),
          ],
        ),
      ),
    );
  }

  Widget _trendCard(
    BuildContext context,
    String title,
    List<int> values,
    Color color,
  ) {
    final t = context.tokens;
    return Card(
      child: Padding(
        padding: const EdgeInsets.all(16),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text(
              title,
              style: TextStyle(
                fontSize: 13.5,
                fontWeight: FontWeight.w600,
                color: t.ink,
              ),
            ),
            const SizedBox(height: 12),
            Spark(values: values, color: color),
          ],
        ),
      ),
    );
  }
}
