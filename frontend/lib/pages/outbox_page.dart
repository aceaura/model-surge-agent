/// 上报待发页：待发与死信两栏，死信可手动重排队。
///
/// 上报失败不影响客户端拿到响应，所以这里堆积不会有人报障——
/// 只能靠这个页面主动发现，否则调度层的运行态会一直缺这些结果。
library;

import 'package:flutter/material.dart';

import '../api_client.dart';
import '../models.dart';
import '../ui/feedback.dart';

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
    return Column(
      children: [
        Padding(
          padding: const EdgeInsets.all(16),
          child: Row(
            children: [
              Text('结果上报队列',
                  style: Theme.of(context).textTheme.titleMedium),
              const Spacer(),
              BusyButton(busy: _loading, onPressed: _load, child: const Text('刷新')),
            ],
          ),
        ),
        Expanded(
          child: Row(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Expanded(
                child: _column(context, '待发 ${_pending.length}', _pending,
                    retryable: false),
              ),
              const VerticalDivider(width: 1),
              Expanded(
                child:
                    _column(context, '死信 ${_dead.length}', _dead, retryable: true),
              ),
            ],
          ),
        ),
      ],
    );
  }

  Widget _column(BuildContext context, String title, List<OutboxEntry> entries,
      {required bool retryable}) {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        Padding(
          padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 8),
          child: Text(title, style: Theme.of(context).textTheme.titleSmall),
        ),
        Expanded(
          child: entries.isEmpty
              ? const Center(child: Text('空'))
              : ListView.separated(
                  padding: const EdgeInsets.symmetric(horizontal: 16),
                  itemCount: entries.length,
                  separatorBuilder: (_, _) => const Divider(height: 1),
                  itemBuilder: (_, i) {
                    final e = entries[i];
                    return ListTile(
                      title: Text(e.reportId),
                      subtitle: Column(
                        crossAxisAlignment: CrossAxisAlignment.start,
                        children: [
                          Text('${e.modelId}  ${e.outcome}  '
                              '尝试 ${e.attempts}  下次 ${stamp(e.nextAttemptAt)}'),
                          if (e.lastError.isNotEmpty)
                            Text(e.lastError,
                                style: TextStyle(
                                    color:
                                        Theme.of(context).colorScheme.error)),
                        ],
                      ),
                      trailing: retryable
                          ? BusyButton(
                              busy: _retrying == e.reportId,
                              onPressed: () => _retry(e),
                              child: const Text('重试'),
                            )
                          : null,
                    );
                  },
                ),
        ),
      ],
    );
  }
}
