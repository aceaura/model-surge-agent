/// 上报待发页：待发与死信两栏，死信可手动重排队。
///
/// 上报失败不影响客户端拿到响应，所以这里堆积不会有人报障——
/// 只能靠这个页面主动发现，否则调度层的运行态会一直缺这些结果。
library;

import 'package:flutter/material.dart';

import '../api_client.dart';
import '../models.dart';
import '../theme.dart';
import '../ui/feedback.dart';
import '../ui/hover_card.dart';

class OutboxPage extends StatefulWidget {
  const OutboxPage({
    super.key,
    required this.client,
    required this.onOpenSettings,
  });

  final ApiClient client;
  final VoidCallback onOpenSettings;

  @override
  State<OutboxPage> createState() => _OutboxPageState();
}

class _OutboxPageState extends State<OutboxPage> {
  List<OutboxEntry> _pending = const [];
  List<OutboxEntry> _dead = const [];
  bool _loading = true;
  String? _retrying;
  Object? _error;

  @override
  void initState() {
    super.initState();
    _load();
  }

  Future<void> _load() async {
    setState(() => _loading = true);
    try {
      final pending = await widget.client.listOutbox(state: 'pending');
      final dead = await widget.client.listOutbox(state: 'dead');
      if (!mounted) return;
      setState(() {
        _pending = pending;
        _dead = dead;
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

  Future<void> _retry(OutboxEntry entry) async {
    setState(() => _retrying = entry.reportId);
    try {
      await widget.client.retryOutbox(entry.reportId);
      if (!mounted) return;
      showInfo(context, '已重排队 ${entry.reportId}');
    } catch (e) {
      if (!mounted) return;
      showError(context, e);
    } finally {
      if (mounted) setState(() => _retrying = null);
      await _load();
    }
  }

  @override
  Widget build(BuildContext context) {
    if (_error != null && _pending.isEmpty && _dead.isEmpty) {
      return ErrorPanel(
        error: _error!,
        onRetry: _load,
        onOpenSettings: widget.onOpenSettings,
      );
    }
    final t = context.tokens;
    return Column(
      children: [
        Padding(
          padding: const EdgeInsets.fromLTRB(24, 20, 24, 14),
          child: Row(
            children: [
              Text(
                '上报队列',
                style: TextStyle(
                  fontSize: 21,
                  fontWeight: FontWeight.w700,
                  color: t.ink,
                ),
              ),
              const SizedBox(width: 8),
              Text(
                '待发 ${_pending.length} · 死信 ${_dead.length}',
                style: TextStyle(fontSize: 12.5, color: t.faint),
              ),
              const Spacer(),
              BusyButton(
                busy: _loading,
                onPressed: _load,
                child: const Text('刷新'),
              ),
            ],
          ),
        ),
        Expanded(
          child: Padding(
            padding: const EdgeInsets.fromLTRB(24, 0, 24, 24),
            child: Row(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Expanded(
                  child: _column(context, '待发', _pending, retryable: false),
                ),
                const SizedBox(width: 16),
                Expanded(child: _column(context, '死信', _dead, retryable: true)),
              ],
            ),
          ),
        ),
      ],
    );
  }

  Widget _column(
    BuildContext context,
    String title,
    List<OutboxEntry> entries, {
    required bool retryable,
  }) {
    final t = context.tokens;
    return Card(
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          Padding(
            padding: const EdgeInsets.fromLTRB(16, 14, 16, 10),
            child: Row(
              children: [
                Text(
                  title,
                  style: TextStyle(
                    fontSize: 13.5,
                    fontWeight: FontWeight.w600,
                    color: t.ink,
                  ),
                ),
                const SizedBox(width: 8),
                Text(
                  '${entries.length}',
                  style: TextStyle(fontSize: 12, color: t.faint),
                ),
              ],
            ),
          ),
          Divider(height: 1, color: t.border),
          Expanded(
            child: entries.isEmpty
                ? Center(
                    child: Text(
                      '空',
                      style: TextStyle(fontSize: 12.5, color: t.faint),
                    ),
                  )
                : ListView.separated(
                    padding: const EdgeInsets.all(12),
                    itemCount: entries.length,
                    separatorBuilder: (_, _) => const SizedBox(height: 10),
                    itemBuilder: (_, i) {
                      final e = entries[i];
                      return HoverCard(
                        child: Padding(
                          padding: const EdgeInsets.symmetric(
                            horizontal: 16,
                            vertical: 12,
                          ),
                          child: Row(
                            crossAxisAlignment: CrossAxisAlignment.start,
                            children: [
                              Expanded(
                                child: Column(
                                  crossAxisAlignment: CrossAxisAlignment.start,
                                  children: [
                                    Text(
                                      e.reportId,
                                      style: TextStyle(
                                        fontSize: 12.5,
                                        fontWeight: FontWeight.w600,
                                        color: t.ink,
                                        fontFamily: AppConst.fontMono,
                                      ),
                                    ),
                                    const SizedBox(height: 4),
                                    Text(
                                      '${e.modelId}  ·  尝试 ${e.attempts}  ·  '
                                      '下次 ${stamp(e.nextAttemptAt)}',
                                      style: TextStyle(
                                        fontSize: 12,
                                        color: t.faint,
                                      ),
                                    ),
                                    if (e.lastError.isNotEmpty) ...[
                                      const SizedBox(height: 4),
                                      Text(
                                        e.lastError,
                                        style: TextStyle(
                                          fontSize: 12,
                                          color: t.danger,
                                        ),
                                      ),
                                    ],
                                  ],
                                ),
                              ),
                              const SizedBox(width: 8),
                              OutcomeChip(outcome: e.outcome),
                              if (retryable) ...[
                                const SizedBox(width: 8),
                                BusyButton(
                                  busy: _retrying == e.reportId,
                                  onPressed: () => _retry(e),
                                  child: const Text('重试'),
                                ),
                              ],
                            ],
                          ),
                        ),
                      );
                    },
                  ),
          ),
        ],
      ),
    );
  }
}
