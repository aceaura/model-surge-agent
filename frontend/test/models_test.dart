import 'package:flutter_test/flutter_test.dart';
import 'package:msa_admin/models.dart';

void main() {
  group('Health', () {
    test('缺字段回 unknown 而不是崩', () {
      final h = Health.fromJson(const {});
      expect(h.status, 'unknown');
      expect(h.ok, isFalse);
      expect(h.cacheDisabled, isFalse);
    });

    test('cache disabled 与 down 是两回事', () {
      expect(
          Health.fromJson(const {'cache': 'disabled'}).cacheDisabled, isTrue);
      expect(Health.fromJson(const {'cache': 'down'}).cacheDisabled, isFalse);
    });
  });

  group('RequestSummary', () {
    test('时间串转本地时区，非法时间为 null', () {
      final r = RequestSummary.fromJson(const {
        'request_id': 'req-1',
        'at': '2026-09-16T01:02:03Z',
        'tried_ids': ['m-a', 'm-b'],
        'committed': true,
      });
      expect(r.at, isNotNull);
      expect(r.at!.isUtc, isFalse);
      expect(r.triedIds, ['m-a', 'm-b']);
      expect(r.committed, isTrue);
      expect(RequestSummary.fromJson(const {'at': 'not-a-time'}).at, isNull);
    });
  });

  group('RequestPage', () {
    test('空游标表示没有下一页', () {
      expect(RequestPage.fromJson(const {}).hasMore, isFalse);
      expect(
          RequestPage.fromJson(const {'next_cursor': 'abc'}).hasMore, isTrue);
    });
  });

  group('Stats', () {
    test('计数 map 与浮点字段整数化解析', () {
      final s = Stats.fromJson(const {
        'window': '1h',
        'buckets': [
          {'minute': '2026-09-16T01:02:00Z', 'total': 3, 'output_tokens': 7}
        ],
        'totals': {
          'total': 3,
          'outcomes': {'normal': 2, 'abnormal': 1},
          'success_rate': 0.6666,
          // qps 在整数值上会被 JSON 解成 int，必须容忍。
          'qps': 1,
        },
      });
      expect(s.buckets.single.total, 3);
      expect(s.buckets.single.outputTokens, 7);
      expect(s.totals.outcomes['normal'], 2);
      expect(s.totals.qps, 1.0);
      expect(s.totals.successRate, closeTo(0.6666, 1e-9));
    });
  });

  group('ModelsPage', () {
    test('协议清单与 outbound_ready 原样带出', () {
      final p = ModelsPage.fromJson(const {
        'models': [
          {'name': 'pool', 'protocol': 'gemini', 'outbound_ready': false}
        ],
        'inbound': ['anthropic'],
        'outbound': ['anthropic', 'gemini'],
        'cached': true,
      });
      expect(p.models.single.outboundReady, isFalse);
      expect(p.outbound, ['anthropic', 'gemini']);
      expect(p.cached, isTrue);
    });
  });
}
