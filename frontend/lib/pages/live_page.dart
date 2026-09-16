/// 实时流水页：2 秒轮询 /admin/live，看最近请求的协议、目标与首字延迟。
library;

import 'dart:async';

import 'package:flutter/material.dart';

import '../api_client.dart';
import '../models.dart';
import '../ui/feedback.dart';

class LiveFeedPage extends StatefulWidget {
  const LiveFeedPage({
    super.key,
    required this.client,
    required this.onOpenSettings,
  });

  final ApiClient client;
  final VoidCallback onOpenSettings;

  @override
  State<LiveFeedPage> createState() => _LiveFeedPageState();
}

class _LiveFeedPageState extends State<LiveFeedPage> {
  LivePage _page = LivePage.empty;
  Object? _error;
  bool _paused = false;
  Timer? _timer;

  @override
  void initState() {
    super.initState();
    _load();
    _timer = Timer.periodic(const Duration(seconds: 2), (_) {
      if (!_paused) _load();
    });
  }

  @override
  void dispose() {
    _timer?.cancel();
    super.dispose();
  }

  Future<void> _load() async {
    try {
      final page = await widget.client.live();
      if (!mounted) return;
      setState(() {
        _page = page;
        _error = null;
      });
    } catch (e) {
      if (!mounted) return;
      setState(() => _error = e);
    }
  }

  @override
  Widget build(BuildContext context) {
    if (_error != null && _page.entries.isEmpty) {
      return ErrorPanel(
        error: _error!,
        onRetry: _load,
        onOpenSettings: widget.onOpenSettings,
      );
    }
    return Column(
      children: [
        if (_page.degraded)
          const DegradedBanner(message: '缓存不可用，这里看不到流水（不等于没有请求）'),
        Padding(
          padding: const EdgeInsets.all(16),
          child: Row(
            children: [
              Text('实时流水（最近 ${_page.entries.length} 条）',
                  style: Theme.of(context).textTheme.titleMedium),
              const Spacer(),
              // 暂停是为了让运维者能停下来读某一行，而不必跟着 2 秒刷新抢。
              TextButton.icon(
                onPressed: () => setState(() => _paused = !_paused),
                icon: Icon(_paused ? Icons.play_arrow : Icons.pause),
                label: Text(_paused ? '继续' : '暂停'),
              ),
              const SizedBox(width: 8),
              IconButton(
                tooltip: '立即刷新',
                onPressed: _load,
                icon: const Icon(Icons.refresh),
              ),
            ],
          ),
        ),
        Expanded(
          child: _page.entries.isEmpty
              ? Center(
                  child: Text(_page.degraded ? '缓存不可用' : '窗口内没有请求',
                      style: Theme.of(context).textTheme.bodyMedium))
              : SingleChildScrollView(
                  padding: const EdgeInsets.symmetric(horizontal: 16),
                  child: SizedBox(
                    width: double.infinity,
                    child: DataTable(
                      columnSpacing: 20,
                      columns: const [
                        DataColumn(label: Text('时间')),
                        DataColumn(label: Text('入站')),
                        DataColumn(label: Text('出站')),
                        DataColumn(label: Text('user_model')),
                        DataColumn(label: Text('model_id')),
                        DataColumn(label: Text('账号')),
                        DataColumn(label: Text('outcome')),
                        DataColumn(label: Text('HTTP')),
                        DataColumn(label: Text('尝试')),
                        DataColumn(label: Text('首字 ms')),
                        DataColumn(label: Text('总耗时 ms')),
                        DataColumn(label: Text('token 入/出')),
                      ],
                      rows: [
                        for (final e in _page.entries)
                          DataRow(cells: [
                            DataCell(Text(stamp(e.at))),
                            DataCell(Text(e.inboundProtocol)),
                            DataCell(Text(e.outboundProtocol)),
                            DataCell(Text(e.userModel)),
                            DataCell(Text(e.modelId.isEmpty ? '-' : e.modelId)),
                            DataCell(Text(e.account.isEmpty ? '-' : e.account)),
                            DataCell(OutcomeChip(outcome: e.outcome)),
                            DataCell(Text('${e.statusCode}')),
                            DataCell(Text('${e.attempts}')),
                            // 0 表示没等到任何帧就结束了（早夭失败），显示 - 与真实的 0ms 区分。
                            DataCell(Text(e.firstTokenMs == 0
                                ? '-'
                                : '${e.firstTokenMs}')),
                            DataCell(Text('${e.latencyMs}')),
                            DataCell(Text(
                                '${compact(e.inputTokens)}/${compact(e.outputTokens)}')),
                          ]),
                      ],
                    ),
                  ),
                ),
        ),
      ],
    );
  }
}
