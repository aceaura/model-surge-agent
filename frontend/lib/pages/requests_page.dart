/// 请求流水页：按 outcome / user_model / model_id 筛选，游标翻页，点行看详情。
library;

import 'package:flutter/material.dart';

import '../api_client.dart';
import '../models.dart';
import '../ui/feedback.dart';

class RequestsPage extends StatefulWidget {
  const RequestsPage({
    super.key,
    required this.client,
    required this.onOpenSettings,
  });

  final ApiClient client;
  final VoidCallback onOpenSettings;

  @override
  State<RequestsPage> createState() => _RequestsPageState();
}

class _RequestsPageState extends State<RequestsPage> {
  final _userModel = TextEditingController();
  final _modelId = TextEditingController();

  String _outcome = '';
  final List<RequestSummary> _rows = [];
  String _nextCursor = '';
  bool _loading = true;
  Object? _error;

  @override
  void initState() {
    super.initState();
    _reload();
  }

  @override
  void dispose() {
    _userModel.dispose();
    _modelId.dispose();
    super.dispose();
  }

  Future<void> _reload() async {
    setState(() {
      _loading = true;
      _rows.clear();
      _nextCursor = '';
    });
    await _fetch();
  }

  /// _fetch 追加一页。游标为空即首页，因此翻页与刷新共用一条路径。
  Future<void> _fetch() async {
    try {
      final page = await widget.client.listRequests(
        cursor: _nextCursor,
        outcome: _outcome,
        userModel: _userModel.text.trim(),
        modelId: _modelId.text.trim(),
      );
      if (!mounted) return;
      setState(() {
        _rows.addAll(page.requests);
        _nextCursor = page.nextCursor;
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

  void _openDetail(RequestSummary row) {
    showDialog<void>(
      context: context,
      builder: (_) => _RequestDetailDialog(row: row),
    );
  }

  @override
  Widget build(BuildContext context) {
    if (_error != null && _rows.isEmpty) {
      return ErrorPanel(
        error: _error!,
        onRetry: _reload,
        onOpenSettings: widget.onOpenSettings,
      );
    }
    return Column(
      children: [
        Padding(
          padding: const EdgeInsets.all(16),
          child: Row(
            children: [
              SizedBox(
                width: 180,
                child: DropdownButtonFormField<String>(
                  initialValue: _outcome,
                  decoration: const InputDecoration(
                      labelText: 'outcome', border: OutlineInputBorder()),
                  items: [
                    const DropdownMenuItem(value: '', child: Text('全部')),
                    for (final o in allOutcomes)
                      DropdownMenuItem(value: o, child: Text(o)),
                  ],
                  onChanged: (v) {
                    setState(() => _outcome = v ?? '');
                    _reload();
                  },
                ),
              ),
              const SizedBox(width: 12),
              SizedBox(
                width: 200,
                child: TextField(
                  controller: _userModel,
                  decoration: const InputDecoration(
                      labelText: 'user_model', border: OutlineInputBorder()),
                  onSubmitted: (_) => _reload(),
                ),
              ),
              const SizedBox(width: 12),
              SizedBox(
                width: 200,
                child: TextField(
                  controller: _modelId,
                  decoration: const InputDecoration(
                      labelText: 'model_id', border: OutlineInputBorder()),
                  onSubmitted: (_) => _reload(),
                ),
              ),
              const SizedBox(width: 12),
              BusyButton(
                busy: _loading,
                onPressed: _reload,
                child: const Text('查询'),
              ),
            ],
          ),
        ),
        Expanded(
          child: _rows.isEmpty
              ? Center(
                  child: _loading
                      ? const CircularProgressIndicator()
                      : const Text('没有符合条件的请求'))
              : SingleChildScrollView(
                  padding: const EdgeInsets.symmetric(horizontal: 16),
                  child: Column(
                    children: [
                      SizedBox(
                        width: double.infinity,
                        child: DataTable(
                          columnSpacing: 20,
                          columns: const [
                            DataColumn(label: Text('时间')),
                            DataColumn(label: Text('request_id')),
                            DataColumn(label: Text('入站/出站')),
                            DataColumn(label: Text('user_model')),
                            DataColumn(label: Text('model_id')),
                            DataColumn(label: Text('outcome')),
                            DataColumn(label: Text('HTTP')),
                            DataColumn(label: Text('尝试')),
                            DataColumn(label: Text('耗时 ms')),
                            DataColumn(label: Text('token 入/出')),
                          ],
                          rows: [
                            for (final r in _rows)
                              DataRow(
                                onSelectChanged: (_) => _openDetail(r),
                                cells: [
                                  DataCell(Text(stamp(r.at))),
                                  DataCell(Text(r.requestId)),
                                  DataCell(Text(
                                      '${r.inboundProtocol} → ${r.outboundProtocol}')),
                                  DataCell(Text(r.userModel)),
                                  DataCell(
                                      Text(r.modelId.isEmpty ? '-' : r.modelId)),
                                  DataCell(OutcomeChip(outcome: r.outcome)),
                                  DataCell(Text('${r.statusCode}')),
                                  DataCell(Text('${r.attempts}')),
                                  DataCell(Text('${r.latencyMs}')),
                                  DataCell(Text(
                                      '${compact(r.inputTokens)}/${compact(r.outputTokens)}')),
                                ],
                              ),
                          ],
                        ),
                      ),
                      const SizedBox(height: 16),
                      if (_nextCursor.isNotEmpty)
                        BusyButton(
                          busy: _loading,
                          onPressed: _fetch,
                          child: const Text('加载更多'),
                        ),
                      const SizedBox(height: 24),
                    ],
                  ),
                ),
        ),
      ],
    );
  }
}

class _RequestDetailDialog extends StatelessWidget {
  const _RequestDetailDialog({required this.row});

  final RequestSummary row;

  @override
  Widget build(BuildContext context) {
    return AlertDialog(
      title: Text('请求 ${row.requestId}'),
      content: SizedBox(
        width: 640,
        child: SingleChildScrollView(
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              _row('时间', stamp(row.at)),
              _row('协议', '${row.inboundProtocol} → ${row.outboundProtocol}'),
              _row('user_model', row.userModel),
              _row('model_id', row.modelId.isEmpty ? '-' : row.modelId),
              _row('账号', row.account.isEmpty ? '-' : row.account),
              _row('outcome', row.outcome),
              _row('HTTP', '${row.statusCode}'),
              _row('尝试次数', '${row.attempts}'),
              // tried_ids 是换目标的链路：排查「为什么打到这个目标」全看它。
              _row('tried_ids',
                  row.triedIds.isEmpty ? '-' : row.triedIds.join(' → ')),
              _row('流式', row.stream ? '是' : '否'),
              // committed 决定了失败为什么是 200：越过它之后错误只能塞进流里。
              _row('已提交响应', row.committed ? '是' : '否'),
              _row('token 入/出/缓存读',
                  '${row.inputTokens} / ${row.outputTokens} / ${row.cacheReadTokens}'
                  '${row.usageEstimated ? '（估算）' : ''}'),
              _row('首字 ms',
                  row.firstTokenMs == 0 ? '-' : '${row.firstTokenMs}'),
              _row('总耗时 ms', '${row.latencyMs}'),
              if (row.errorCode.isNotEmpty) _row('错误码', row.errorCode),
              if (row.errorMessage.isNotEmpty)
                Padding(
                  padding: const EdgeInsets.only(top: 8),
                  child: SelectableText(row.errorMessage,
                      style: TextStyle(
                          color: Theme.of(context).colorScheme.error)),
                ),
            ],
          ),
        ),
      ),
      actions: [
        TextButton(
          onPressed: () => Navigator.of(context).pop(),
          child: const Text('关闭'),
        ),
      ],
    );
  }

  Widget _row(String label, String value) => Padding(
        padding: const EdgeInsets.symmetric(vertical: 4),
        child: Row(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            SizedBox(width: 140, child: Text(label)),
            Expanded(child: SelectableText(value)),
          ],
        ),
      );
}
