function initChart(id) {
  const canvas = document.getElementById(id);
  if (!canvas) return;

  const labels   = JSON.parse(canvas.dataset.labels);
  const datasets = JSON.parse(canvas.dataset.datasets);
  const isDense  = labels.length > 100;

  const existing = Chart.getChart(id);
  if (existing) existing.destroy();

  const css = name => getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  const textColor   = css('--text');
  const gridColor   = css('--text-muted');
  const borderColor = css('--text');

  function simpleNumbers(val) {
    if (val >= 1_000_000) return (val / 1_000_000).toFixed(1) + 'M';
    if (val >= 1_000)     return (val / 1_000).toFixed(1) + 'k';
    return val;
  }

  Chart.defaults.font.family = "Ubuntu";
  Chart.defaults.color = textColor;

  new Chart(canvas, {
    type: 'line',
    data: {
      labels,
      datasets: datasets.map(d => ({
        ...d,
        borderWidth:     2,
        pointRadius:     isDense ? 0 : 3,
        pointHoverRadius: 4,
        tension:         0.3,
        fill:            true,
      })),
    },
    options: {
      aspectRatio: 3,
      maintainAspectRatio: true,
      interaction: { mode: 'index', intersect: false },
      plugins: {
        legend: {
          labels: {
            color:     textColor,
            font:      { size: 14 },
            boxWidth:  32,
            padding:   16,
            usePointStyle: true,
            pointStyle: 'line',
          },
        },
        tooltip: {
          backgroundColor: css('--surface'),
          titleColor:      css('--text'),
          bodyColor:       css('--text'),
          borderColor,
          borderWidth:     1,
          padding: 10,
          usePointStyle: true,
          callbacks: {
            labelPointStyle: function(context) {
              return { pointStyle: "line" }
            }
          }
        },
      },
      scales: {
        x: {
          ticks: {
            maxTicksLimit: 6,
            color:         textColor,
            callback: function(val, index) {
              return index % 2 === 1 ? this.getLabelForValue(val) : '';
            },
          },
          border: { color: borderColor },
          grid:   { color: gridColor, lineWidth: 0.5 },
        },
        y: {
          beginAtZero: true,
          ticks: {
            maxTicksLimit: 10,
            color:         textColor,
            callback:      simpleNumbers,
          },
          border: { color: borderColor },
          grid:   { color: gridColor, lineWidth: 0.5 },
        },
      },
    },
  });
}
