/// 总览页：窗口内的 QPS、成功率、outcome 分布与 token 曲线，外加健康状态。
library;

import 'dart:async';

import 'package:flutter/material.dart';

import '../api_client.dart';
import '../models.dart';
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
    return Column(
      children: [
        if (_stats.degraded)
          const DegradedBanner(message: '缓存不可用，统计为空不代表没有流量'),
        Padding(
          padding: const EdgeInsets.all(16),
          child: Row(
            children: [
              Text('统计窗口', style: Theme.of(context).textTheme.titleMedium),
              const SizedBox(width: 16),
              SegmentedButton<String>(
                segments: [
                  for (final w in _windows)
                    ButtonSegment(value: w, label: Text(w)),
                ],
                selected: {_window},
                onSelectionChanged: (s) => _pick(s.first),
              ),
              const Spacer(),
              if (_loading)
                const SizedBox(
                    width: 16,
                    height: 16,
                    child: CircularProgressIndicator(strokeWidth: 2)),
            ],
          ),
        ),
        Expanded(
          child: ListView(
            padding: const EdgeInsets.symmetric(horizontal: 16),
            children: [
              _healthCard(context),
              const SizedBox(height: 16),
              _totalsRow(context),
              const SizedBox(height: 16),
              _outcomesCard(context),
              const SizedBox(height: 16),
              _trendCard(context, '请求量（每分钟）',
                  [for (final b in _stats.buckets) b.total],
                  Theme.of(context).colorScheme.primary),
              const SizedBox(height: 16),
              _trendCard(context, '输出 token（每分钟）',
                  [for (final b in _stats.buckets) b.outputTokens],
                  Theme.of(context).colorScheme.tertiary),
              const SizedBox(height: 16),
              _trendCard(context, '平均延迟 ms（每分钟）',
                  [for (final b in _stats.buckets) b.avgLatencyMs],
                  Theme.of(context).colorScheme.secondary),
              const SizedBox(height: 24),
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
    final scheme = Theme.of(context).colorScheme;
    // disabled 用中性色：它是部署选择而不是故障。
    final color = switch (state) {
      'ok' => scheme.primary,
      'disabled' => scheme.outline,
      _ => scheme.error,
    };
    return Row(
      mainAxisSize: MainAxisSize.min,
      children: [
        Icon(Icons.circle, size: 10, color: color),
        const SizedBox(width: 6),
        Text('$label $state'),
      ],
    );
  }

  Widget _pair(String label, String value) => Row(
        mainAxisSize: MainAxisSize.min,
        children: [Text('$label '), Text(value)],
      );

  Widget _totalsRow(BuildContext context) {
    final t = _stats.totals;
    return Row(
      children: [
        _metric(context, '请求数', '${t.total}'),
        _metric(context, 'QPS', t.qps.toStringAsFixed(3)),
        _metric(context, '成功率',
            t.total == 0 ? '-' : '${(t.successRate * 100).toStringAsFixed(1)}%'),
        _metric(context, '输入 token', compact(t.inputTokens)),
        _metric(context, '输出 token', compact(t.outputTokens)),
      ],
    );
  }

  Widget _metric(BuildContext context, String label, String value) {
    return Expanded(
      child: Card(
        child: Padding(
          padding: const EdgeInsets.symmetric(vertical: 16, horizontal: 12),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text(label, style: Theme.of(context).textTheme.bodySmall),
              const SizedBox(height: 4),
              Text(value, style: Theme.of(context).textTheme.headlineSmall),
            ],
          ),
        ),
      ),
    );
  }

  Widget _outcomesCard(BuildContext context) {
    final counts = _stats.totals.outcomes;
    return Card(
      child: Padding(
        padding: const EdgeInsets.all(16),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text('outcome 分布',
                style: Theme.of(context).textTheme.titleMedium),
            const SizedBox(height: 12),
            Wrap(
              spacing: 16,
              runSpacing: 8,
              children: [
                for (final o in allOutcomes)
                  Row(
                    mainAxisSize: MainAxisSize.min,
                    children: [
                      OutcomeChip(outcome: o),
                      const SizedBox(width: 6),
                      Text('${counts[o] ?? 0}'),
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
      BuildContext context, String title, List<int> values, Color color) {
    return Card(
      child: Padding(
        padding: const EdgeInsets.all(16),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text(title, style: Theme.of(context).textTheme.titleMedium),
            const SizedBox(height: 12),
            Spark(values: values, color: color),
          ],
        ),
      ),
    );
  }
}
