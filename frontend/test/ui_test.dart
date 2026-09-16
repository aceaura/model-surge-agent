import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:msa_admin/api_client.dart';
import 'package:msa_admin/settings_store.dart';
import 'package:msa_admin/ui/feedback.dart';
import 'package:msa_admin/ui/spark.dart';

void main() {
  group('describeError', () {
    test('不可达带上所用地址与排查方向', () {
      const e = UnreachableException(
          'http://agent.test', '无法连接服务：http://agent.test');
      expect(describeError(e), contains('http://agent.test'));
      expect(describeError(e), contains('确认服务已启动'));
    });

    test('密钥无效引导去设置页', () {
      expect(describeError(const UnauthorizedException()), contains('设置页'));
    });

    test('服务端错误直接展示其 message', () {
      expect(describeError(const ApiErrorException('invalid_request', 'bad', 400)),
          'bad');
    });
  });

  group('needsSettings', () {
    test('只有连接类失败才引导改配置', () {
      expect(needsSettings(const UnauthorizedException()), isTrue);
      expect(needsSettings(const UnreachableException('u', 'm')), isTrue);
      expect(needsSettings(const ApiErrorException('invalid_request', 'x', 400)),
          isFalse);
    });
  });

  group('mask', () {
    test('短密钥全掩，长密钥留首尾', () {
      expect(mask(''), '');
      expect(mask('short'), '***');
      expect(mask('abcdefghijkl'), 'abcd***ijkl');
    });
  });

  group('compact', () {
    test('按量级缩写，小数值原样', () {
      expect(compact(999), '999');
      expect(compact(1500), '1.5k');
      expect(compact(2500000), '2.5M');
    });
  });

  group('stamp', () {
    test('null 显示占位符', () {
      expect(stamp(null), '-');
      expect(stamp(DateTime(2026, 9, 16, 1, 2, 3)), '09-16 01:02:03');
    });
  });

  testWidgets('outcomeColor 把 context_exceeded 与真失败区分开', (tester) async {
    late Color exceeded;
    late Color abnormal;
    late Color normal;
    await tester.pumpWidget(MaterialApp(
      home: Builder(builder: (context) {
        exceeded = outcomeColor(context, 'context_exceeded');
        abnormal = outcomeColor(context, 'abnormal');
        normal = outcomeColor(context, 'normal');
        return const SizedBox();
      }),
    ));
    // 输入太长是客户端的问题，不该和上游失败共用错误色。
    expect(exceeded, isNot(abnormal));
    expect(normal, isNot(abnormal));
  });

  testWidgets('DegradedBanner 显示传入的说明', (tester) async {
    await tester.pumpWidget(const MaterialApp(
      home: Scaffold(body: DegradedBanner(message: '缓存不可用')),
    ));
    expect(find.text('缓存不可用'), findsOneWidget);
  });

  testWidgets('BusyButton 进行中禁用自身', (tester) async {
    var taps = 0;
    await tester.pumpWidget(MaterialApp(
      home: Scaffold(
        body: BusyButton(busy: true, onPressed: () => taps++, child: const Text('go')),
      ),
    ));
    await tester.tap(find.byType(FilledButton));
    expect(taps, 0);
  });

  testWidgets('Spark 空数据给出文字提示而不是空白', (tester) async {
    await tester.pumpWidget(const MaterialApp(
      home: Scaffold(body: Spark(values: [], color: Colors.blue)),
    ));
    expect(find.text('窗口内没有数据'), findsOneWidget);
  });

  testWidgets('Spark 全零窗口不崩（除零）', (tester) async {
    await tester.pumpWidget(const MaterialApp(
      home: Scaffold(body: Spark(values: [0, 0, 0], color: Colors.blue)),
    ));
    expect(tester.takeException(), isNull);
  });
}
